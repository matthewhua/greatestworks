// Package runtime contains code suitable for deployer implementers but not
// Service Weaver application developers.
package aop

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
)

const (
	// ToWeaveletKey is the environment variable under which the file descriptor
	// for messages sent from envelope to weavelet is stored. For internal use by
	// Service Weaver infrastructure.
	ToWeaveletKey = "ENVELOPE_TO_WEAVELET_FD"

	// ToEnvelopeKey is the environment variable under which the file descriptor
	// for messages sent from weavelet to envelope is stored. For internal use by
	// Service Weaver infrastructure.
	ToEnvelopeKey = "WEAVELET_TO_ENVELOPE_FD"
)

// Bootstrap holds configuration information used to start a process execution.
type Bootstrap struct {
	ToWeaveletFd int    // File descriptor on which to send to weavelet (0 if unset)
	ToEnvelopeFd int    // File descriptor from which to send to envelope (0 if unset)
	TestConfig   string // Configuration passed by user test code to weavertest
}

// BootstrapKey is the Context key used by weavertest to pass Bootstrap to [weaver.Init].
type BootstrapKey struct{}

// GetBootstrap returns information needed to configure process
// execution. For normal execution, this comes from the environment. For
// weavertest, it comes from a context value.
func GetBootstrap(ctx context.Context) (Bootstrap, error) {
	if val := ctx.Value(BootstrapKey{}); val != nil {
		bootstrap, ok := val.(Bootstrap)
		if !ok {
			return Bootstrap{}, fmt.Errorf("invalid type %T for bootstrap info in context", val)
		}
		return bootstrap, nil
	}

	str1 := os.Getenv(ToWeaveletKey)
	str2 := os.Getenv(ToEnvelopeKey)
	if str1 == "" && str2 == "" {
		return Bootstrap{}, nil
	}
	if str1 == "" || str2 == "" {
		return Bootstrap{}, fmt.Errorf("envelope/weavelet pipe should have 2 file descriptors, got (%s, %s)", str1, str2)
	}
	toWeaveletFd, err := strconv.Atoi(str1)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("unable to parse envelope to weavelet fd: %w", err)
	}
	toEnvelopeFd, err := strconv.Atoi(str2)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("unable to parse weavelet to envelope fd: %w", err)
	}
	return Bootstrap{
		ToWeaveletFd: toWeaveletFd,
		ToEnvelopeFd: toEnvelopeFd,
	}, nil
}

// HasPipes returns true if pipe information has been supplied. This
// is true except in the case of singleprocess.
func (b Bootstrap) HasPipes() bool {
	return b.ToWeaveletFd != 0 && b.ToEnvelopeFd != 0
}

// MakePipes creates pipe reader and writer. It returns an error if pipes are not configured.
func (b Bootstrap) MakePipes() (io.ReadCloser, io.WriteCloser, error) {
	toWeavelet, err := openFileDescriptor(b.ToWeaveletFd)
	if err != nil {
		return nil, nil, fmt.Errorf("open pipe to weavelet: %w", err)
	}
	toEnvelope, err := openFileDescriptor(b.ToEnvelopeFd)
	if err != nil {
		return nil, nil, fmt.Errorf("open pipe to envelope: %w", err)
	}
	return toWeavelet, toEnvelope, nil
}

func openFileDescriptor(fd int) (*os.File, error) {
	if fd == 0 {
		return nil, fmt.Errorf("bad file descriptor %d", fd)
	}
	f := os.NewFile(uintptr(fd), fmt.Sprint("/proc/self/fd/", fd))
	if f == nil {
		return nil, fmt.Errorf("open file descriptor %d: failed", fd)
	}
	return f, nil
}
