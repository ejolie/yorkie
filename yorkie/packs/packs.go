/*
 * Copyright 2020 The Yorkie Authors. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package packs

import (
	"context"
	"fmt"
	gotime "time"

	"github.com/yorkie-team/yorkie/pkg/document/change"
	"github.com/yorkie-team/yorkie/pkg/document/key"
	"github.com/yorkie-team/yorkie/pkg/document/time"
	"github.com/yorkie-team/yorkie/pkg/types"
	"github.com/yorkie-team/yorkie/yorkie/backend"
	"github.com/yorkie-team/yorkie/yorkie/backend/db"
	"github.com/yorkie-team/yorkie/yorkie/backend/sync"
	"github.com/yorkie-team/yorkie/yorkie/logging"
)

// NewPushPullKey creates a new sync.Key of PushPull for the given document.
func NewPushPullKey(documentKey *key.Key) sync.Key {
	return sync.NewKey(fmt.Sprintf("pushpull-%s", documentKey.BSONKey()))
}

// NewSnapshotKey creates a new sync.Key of Snapshot for the given document.
func NewSnapshotKey(documentKey *key.Key) sync.Key {
	return sync.NewKey(fmt.Sprintf("snapshot-%s", documentKey.BSONKey()))
}

// PushPull stores the given changes and returns accumulated changes of the
// given document.
func PushPull(
	ctx context.Context,
	be *backend.Backend,
	clientInfo *db.ClientInfo,
	docInfo *db.DocInfo,
	reqPack *change.Pack,
) (*ServerPack, error) {
	start := gotime.Now()
	defer func() {
		be.Metrics.ObservePushPullResponseSeconds(gotime.Since(start).Seconds())
	}()

	// TODO: Changes may be reordered or missing during communication on the network.
	// We should check the change.pack with checkpoint to make sure the changes are in the correct order.
	initialServerSeq := docInfo.ServerSeq

	// 01. push changes.
	pushedCP, pushedChanges, err := pushChanges(ctx, clientInfo, docInfo, reqPack, initialServerSeq)
	if err != nil {
		return nil, err
	}
	be.Metrics.AddPushPullReceivedChanges(reqPack.ChangesLen())
	be.Metrics.AddPushPullReceivedOperations(reqPack.OperationsLen())

	// 02. pull change pack.
	respPack, err := pullPack(ctx, be, clientInfo, docInfo, reqPack, pushedCP, initialServerSeq)
	if err != nil {
		return nil, err
	}
	be.Metrics.AddPushPullSentChanges(respPack.ChangesLen())
	be.Metrics.AddPushPullSentOperations(respPack.OperationsLen())
	be.Metrics.AddPushPullSnapshotBytes(respPack.SnapshotLen())

	if err := clientInfo.UpdateCheckpoint(docInfo.ID, respPack.Checkpoint); err != nil {
		return nil, err
	}

	// 03. store pushed changes, document info and checkpoint of the client to DB.
	if len(pushedChanges) > 0 {
		if err := be.DB.CreateChangeInfos(ctx, docInfo, initialServerSeq, pushedChanges); err != nil {
			return nil, err
		}
	}

	if err := be.DB.UpdateClientInfoAfterPushPull(ctx, clientInfo, docInfo); err != nil {
		return nil, err
	}

	// 04. update and find min synced ticket for garbage collection.
	// NOTE(hackerwins): Since the client could not receive the response, the
	// requested seq(reqPack) is stored instead of the response seq(resPack).
	minSyncedTicket, err := be.DB.UpdateAndFindMinSyncedTicket(
		ctx,
		clientInfo,
		docInfo.ID,
		reqPack.Checkpoint.ServerSeq,
	)
	if err != nil {
		return nil, err
	}
	respPack.MinSyncedTicket = minSyncedTicket

	// 05. publish document change event then store snapshot asynchronously.
	if reqPack.HasChanges() {
		be.Background.AttachGoroutine(func(ctx context.Context) {
			publisherID, err := time.ActorIDFromHex(clientInfo.ID.String())
			if err != nil {
				logging.From(ctx).Error(err)
				return
			}

			locker, err := be.Coordinator.NewLocker(
				ctx,
				NewSnapshotKey(reqPack.DocumentKey),
			)
			if err != nil {
				logging.From(ctx).Error(err)
				return
			}
			// NOTE: If the snapshot is already being created by another routine, it
			//       is not necessary to recreate it, so we can skip it.
			if err := locker.TryLock(ctx); err != nil {
				return
			}

			defer func() {
				if err := locker.Unlock(ctx); err != nil {
					logging.From(ctx).Error(err)
					return
				}
			}()

			be.Coordinator.Publish(
				ctx,
				publisherID,
				sync.DocEvent{
					Type:         types.DocumentsChangedEvent,
					Publisher:    types.Client{ID: publisherID},
					DocumentKeys: []*key.Key{reqPack.DocumentKey},
				},
			)

			start := gotime.Now()
			if err := storeSnapshot(
				ctx,
				be,
				docInfo,
				minSyncedTicket,
			); err != nil {
				logging.From(ctx).Error(err)
			}
			be.Metrics.ObservePushPullSnapshotDurationSeconds(
				gotime.Since(start).Seconds(),
			)
		})
	}

	return respPack, nil
}
