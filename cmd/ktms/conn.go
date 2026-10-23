package main

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// target is where and how to reach Keystone's Admin API.
type target struct {
	addr    string
	tls     bool
	keyFile string
}

// adminKey reads the key from KTMS_ADMIN_KEY or a file, never from a flag,
// so it stays out of shell history and process listings.
func (t target) adminKey() (string, error) {
	if t.keyFile != "" {
		b, err := os.ReadFile(t.keyFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	if k := os.Getenv("KTMS_ADMIN_KEY"); k != "" {
		return k, nil
	}
	return "", errors.New("no admin key: set KTMS_ADMIN_KEY or pass --key-file")
}

type bearer struct {
	key    string
	secure bool
}

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.key}, nil
}
func (b bearer) RequireTransportSecurity() bool { return b.secure }

func (t target) admin() (pb.AdminServiceClient, func(), error) {
	key, err := t.adminKey()
	if err != nil {
		return nil, nil, err
	}
	creds := insecure.NewCredentials()
	if t.tls {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(t.addr, grpc.WithTransportCredentials(creds), grpc.WithPerRPCCredentials(bearer{key: key, secure: t.tls}))
	if err != nil {
		return nil, nil, err
	}
	return pb.NewAdminServiceClient(conn), func() { _ = conn.Close() }, nil
}
