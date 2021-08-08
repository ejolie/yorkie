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

package rpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	grpcmiddleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpcprometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/yorkie-team/yorkie/api"
	"github.com/yorkie-team/yorkie/internal/log"
	"github.com/yorkie-team/yorkie/yorkie/backend"
	"github.com/yorkie-team/yorkie/yorkie/rpc/interceptors"
)

var (
	// ErrInvalidRPCPort occurs when the port in the config is invalid.
	ErrInvalidRPCPort = errors.New("invalid port number for RPC server")
	// ErrInvalidCertFile occurs when the certificate file is invalid.
	ErrInvalidCertFile = errors.New("invalid cert file for RPC server")
	// ErrInvalidKeyFile occurs when the key file is invalid.
	ErrInvalidKeyFile = errors.New("invalid key file for RPC server")
)

// Config is the configuration for creating a Server instance.
type Config struct {
	Port     int
	CertFile string
	KeyFile  string
}

// Server is a normal server that processes the logic requested by the client.
type Server struct {
	conf                *Config
	grpcServer          *grpc.Server
	yorkieServiceCancel context.CancelFunc
}

// NewServer creates a new instance of Server.
func NewServer(conf *Config, be *backend.Backend) (*Server, error) {
	authInterceptor := interceptors.NewAuthInterceptor(be.Config.AuthorizationWebhookURL)
	defaultInterceptor := interceptors.NewDefaultInterceptor()

	opts := []grpc.ServerOption{
		grpc.UnaryInterceptor(grpcmiddleware.ChainUnaryServer(
			authInterceptor.Unary(),
			defaultInterceptor.Unary(),
			grpcprometheus.UnaryServerInterceptor,
		)),
		grpc.StreamInterceptor(grpcmiddleware.ChainStreamServer(
			authInterceptor.Stream(),
			defaultInterceptor.Stream(),
			grpcprometheus.StreamServerInterceptor,
		)),
	}

	if conf.CertFile != "" && conf.KeyFile != "" {
		creds, err := credentials.NewServerTLSFromFile(conf.CertFile, conf.KeyFile)
		if err != nil {
			log.Logger.Error(err)
			return nil, err
		}
		opts = append(opts, grpc.Creds(creds))
	}

	yorkieServiceCtx, yorkieServiceCancel := context.WithCancel(context.Background())

	grpcServer := grpc.NewServer(opts...)
	healthpb.RegisterHealthServer(grpcServer, health.NewServer())
	api.RegisterYorkieServer(grpcServer, newYorkieServer(yorkieServiceCtx, be))
	api.RegisterClusterServer(grpcServer, newClusterServer(be))
	grpcprometheus.Register(grpcServer)

	return &Server{
		conf:                conf,
		grpcServer:          grpcServer,
		yorkieServiceCancel: yorkieServiceCancel,
	}, nil
}

// Start starts this server by opening the rpc port.
func (s *Server) Start() error {
	return s.listenAndServeGRPC()
}

// Shutdown shuts down this server.
func (s *Server) Shutdown(graceful bool) {
	s.yorkieServiceCancel()

	if graceful {
		s.grpcServer.GracefulStop()
	} else {
		s.grpcServer.Stop()
	}
}

func (s *Server) listenAndServeGRPC() error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", s.conf.Port))
	if err != nil {
		log.Logger.Error(err)
		return err
	}

	go func() {
		log.Logger.Infof("serving API on %d", s.conf.Port)

		if err := s.grpcServer.Serve(lis); err != nil {
			if err != grpc.ErrServerStopped {
				log.Logger.Error(err)
			}
		}
	}()

	return nil
}

// Validate validates the port number and the files for certification.
func (c *Config) Validate() error {
	if c.Port < 1 || 65535 < c.Port {
		return fmt.Errorf("must be between 1 and 65535, given %d: %w", c.Port, ErrInvalidRPCPort)
	}

	// when specific cert or key file are configured
	if c.CertFile != "" {
		if _, err := os.Stat(c.CertFile); err != nil {
			return fmt.Errorf("%s: %w", c.CertFile, ErrInvalidCertFile)
		}
	}

	if c.KeyFile != "" {
		if _, err := os.Stat(c.KeyFile); err != nil {
			return fmt.Errorf("%s: %w", c.KeyFile, ErrInvalidKeyFile)
		}
	}

	return nil
}
