package main

import (
	"context"
	"os"
	"testing"
)

type fakeReadFileFunc func(name string) ([]byte, error)

func TestCheckConfidentialGPUEnabled(t *testing.T) {
	testcases := []struct {
		name         string
		readFileFunc fakeReadFileFunc
		wantEnabled  bool
		wantErr      bool
	}{
		{
			name: "file not found",
			readFileFunc: func(name string) ([]byte, error) {
				return nil, os.ErrNotExist
			},
			wantEnabled: false,
		},
		{
			name: "failed to read file",
			readFileFunc: func(name string) ([]byte, error) {
				return nil, os.ErrPermission
			},
			wantErr: true,
		},
		{
			name: "empty file",
			readFileFunc: func(name string) ([]byte, error) {
				return []byte{}, nil
			},
			wantEnabled: false,
		},
		{
			name: "tdx",
			readFileFunc: func(name string) ([]byte, error) {
				return []byte("TDX"), nil
			},
			wantEnabled: true,
		},
		{
			name: "snp_sev",
			readFileFunc: func(name string) ([]byte, error) {
				return []byte("snp_sev"), nil
			},
			wantEnabled: false,
		},
		{
			name: "trailing spaces - enabled",
			readFileFunc: func(name string) ([]byte, error) {
				return []byte("tdx  "), nil
			},
			wantEnabled: true,
		},
		{
			name: "trailing spaces - disabled",
			readFileFunc: func(name string) ([]byte, error) {
				return []byte("other  "), nil
			},
			wantEnabled: false,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			readFile = tc.readFileFunc

			enabled, err := checkConfidentialGPUEnablement(context.Background())

			if err != nil && !tc.wantErr {
				t.Errorf("checkConfidentialGPUEnablement returned unexpected error %v", err)
			}

			if enabled != tc.wantEnabled {
				t.Errorf("checkConfidentialGPUEnablement returned unexpected enablement: want %v, got %v", tc.wantEnabled, enabled)
			}
		})
	}
}
