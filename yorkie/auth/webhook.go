/*
 * Copyright 2021 The Yorkie Authors. All rights reserved.
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

package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"syscall"
	"time"

	"github.com/yorkie-team/yorkie/pkg/document/change"
	"github.com/yorkie-team/yorkie/pkg/types"
	"github.com/yorkie-team/yorkie/yorkie/backend"
	"github.com/yorkie-team/yorkie/yorkie/logging"
)

var (
	// ErrNotAllowed is returned when the given user is not allowed for the access.
	ErrNotAllowed = errors.New("method is not allowed for this user")

	// ErrUnexpectedStatusCode is returned when the response code is not 200 from the webhook.
	ErrUnexpectedStatusCode = errors.New("unexpected status code from webhook")

	// ErrWebhookTimeout is returned when the webhook does not respond in time.
	ErrWebhookTimeout = errors.New("webhook timeout")
)

// AccessAttributes returns an array of AccessAttribute from the given pack.
func AccessAttributes(pack *change.Pack) []types.AccessAttribute {
	verb := types.Read
	if pack.HasChanges() {
		verb = types.ReadWrite
	}

	// NOTE(hackerwins): In the future, methods such as bulk PushPull can be
	// added, so we declare it as an array.
	return []types.AccessAttribute{{
		Key:  pack.DocumentKey.BSONKey(),
		Verb: verb,
	}}
}

// VerifyAccess verifies the given access.
func VerifyAccess(ctx context.Context, be *backend.Backend, info *types.AccessInfo) error {
	if !be.Config.RequireAuth(info.Method) {
		return nil
	}

	reqBody, err := json.Marshal(types.AuthWebhookRequest{
		Token:      TokenFromCtx(ctx),
		Method:     info.Method,
		Attributes: info.Attributes,
	})
	if err != nil {
		return err
	}

	cacheKey := string(reqBody)
	if entry, ok := be.AuthWebhookCache.Get(cacheKey); ok {
		resp := entry.(*types.AuthWebhookResponse)
		if !resp.Allowed {
			return fmt.Errorf("%s: %w", resp.Reason, ErrNotAllowed)
		}
		return nil
	}

	var authResp *types.AuthWebhookResponse
	if err := withExponentialBackoff(ctx, be.Config, func() (int, error) {
		resp, err := http.Post(
			be.Config.AuthWebhookURL,
			"application/json",
			bytes.NewBuffer(reqBody),
		)
		if err != nil {
			return 0, err
		}

		defer func() {
			if err := resp.Body.Close(); err != nil {
				logging.From(ctx).Error(err)
			}
		}()

		if http.StatusOK != resp.StatusCode {
			return resp.StatusCode, ErrUnexpectedStatusCode
		}

		authResp, err = types.NewAuthWebhookResponse(resp.Body)
		if err != nil {
			return resp.StatusCode, err
		}

		if !authResp.Allowed {
			return resp.StatusCode, fmt.Errorf("%s: %w", authResp.Reason, ErrNotAllowed)
		}

		return resp.StatusCode, nil
	}); err != nil {
		if errors.Is(err, ErrNotAllowed) {
			be.AuthWebhookCache.Add(cacheKey, authResp, be.Config.ParseAuthWebhookCacheUnauthTTL())
		}

		return err
	}

	be.AuthWebhookCache.Add(cacheKey, authResp, be.Config.ParseAuthWebhookCacheAuthTTL())

	return nil
}

func withExponentialBackoff(ctx context.Context, cfg *backend.Config, webhookFn func() (int, error)) error {
	var retries uint64
	var statusCode int
	for retries <= cfg.AuthWebhookMaxRetries {
		statusCode, err := webhookFn()
		if !shouldRetry(statusCode, err) {
			if err == ErrUnexpectedStatusCode {
				return fmt.Errorf("unexpected status code from webhook: %d", statusCode)
			}

			return err
		}

		waitBeforeRetry := waitInterval(retries, cfg.ParseAuthWebhookMaxWaitInterval())

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitBeforeRetry):
		}

		retries++
	}

	return fmt.Errorf("unexpected status code from webhook %d: %w", statusCode, ErrWebhookTimeout)
}

// waitInterval returns the interval of given retries. (2^retries * 100) milliseconds.
func waitInterval(retries uint64, maxWaitInterval time.Duration) time.Duration {
	interval := time.Duration(math.Pow(2, float64(retries))) * 100 * time.Millisecond
	if maxWaitInterval < interval {
		return maxWaitInterval
	}

	return interval
}

// shouldRetry returns true if the given error should be retried.
// Refer to https://github.com/kubernetes/kubernetes/search?q=DefaultShouldRetry
func shouldRetry(statusCode int, err error) bool {
	// If the connection is reset, we should retry.
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.ECONNRESET
	}

	return statusCode == http.StatusInternalServerError ||
		statusCode == http.StatusServiceUnavailable ||
		statusCode == http.StatusGatewayTimeout ||
		statusCode == http.StatusTooManyRequests
}
