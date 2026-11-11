// This file is part of arduino-router.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package msgpackrpc

import (
	"bytes"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMaxIncomingMessageSizeDefault(t *testing.T) {
	c := NewConnection(nil, nil, nil, nil, nil)
	require.Equal(t, int64(DefaultMaxIncomingMessageSize), c.maxIncomingMessageSize.Load())
}

func TestDecodeRpcPacketMaxIncomingMessageSize(t *testing.T) {
	msg := encodeStream(t, []any{messageTypeNotification, "m", []any{strings.Repeat("x", 100)}})
	next := encodeStream(t, []any{messageTypeNotification, "next", []any{}})
	stream := append(append([]byte{}, msg...), next...)

	t.Run("ExactlyAtLimit", func(t *testing.T) {
		c := &Connection{}
		c.SetMaxIncomingMessageSize(int64(len(msg)))
		in := bytes.NewReader(stream)
		packet, err := c.decodeRpcPacket(in)
		require.NoError(t, err)
		require.Equal(t, "m", packet.method)

		// The budget is reset for each packet
		packet, err = c.decodeRpcPacket(in)
		require.NoError(t, err)
		require.Equal(t, "next", packet.method)
	})

	t.Run("OverLimit", func(t *testing.T) {
		c := &Connection{}
		c.SetMaxIncomingMessageSize(int64(len(msg) - 1))
		in := bytes.NewReader(stream)
		_, err := c.decodeRpcPacket(in)
		require.ErrorIs(t, err, ErrMessageTooLarge)
		var invalid *invalidRPCMessageError
		require.NotErrorAs(t, err, &invalid, "must not be treated as a skippable invalid packet")
		// No byte after the limit has been read from the stream
		require.Equal(t, len(stream)-len(msg)+1, in.Len())
	})

	t.Run("OverLimitWhileSkippingInvalidPacket", func(t *testing.T) {
		invalidMsg := encodeStream(t, []any{messageTypeRequest, 1, "m", []any{}, strings.Repeat("x", 100)})
		c := &Connection{}
		c.SetMaxIncomingMessageSize(50)
		_, err := c.decodeRpcPacket(bytes.NewReader(invalidMsg))
		require.ErrorIs(t, err, ErrMessageTooLarge)
		var invalid *invalidRPCMessageError
		require.NotErrorAs(t, err, &invalid)
	})

	t.Run("NoLimit", func(t *testing.T) {
		big := encodeStream(t, []any{messageTypeNotification, "m", []any{strings.Repeat("x", 2*DefaultMaxIncomingMessageSize)}})
		c := NewConnection(nil, nil, nil, nil, nil)
		_, err := c.decodeRpcPacket(bytes.NewReader(big))
		require.ErrorIs(t, err, ErrMessageTooLarge)

		c.SetMaxIncomingMessageSize(0)
		packet, err := c.decodeRpcPacket(bytes.NewReader(big))
		require.NoError(t, err)
		require.Len(t, packet.params[0], 2*DefaultMaxIncomingMessageSize)
	})
}

func TestDecodeRpcPacketLyingParamsLength(t *testing.T) {
	// A notification declaring 2^32-1 params, followed by just one.
	data := []byte{
		0x93,      // fixarray(3)
		0x02,      // notification
		0xa1, 'm', // method "m"
		0xdd, 0xff, 0xff, 0xff, 0xff, // array32 with 4294967295 elements
		0x01,
	}
	for _, raw := range []bool{false, true} {
		c := &Connection{}
		c.SetKeepParamsAndResponsesAsRaw(raw)

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		_, err := decodePacketFromBytes(c, data)
		runtime.ReadMemStats(&after)

		require.Error(t, err)
		require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1024*1024))
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestRunClosesOnMessageTooLarge(t *testing.T) {
	stream := encodeStream(t,
		[]any{messageTypeNotification, "small", []any{}},
		[]any{messageTypeNotification, "big", []any{strings.Repeat("x", 100)}},
		[]any{messageTypeNotification, "after", []any{}},
	)

	var mu sync.Mutex
	var notifications []string
	var errs []error
	c := NewConnection(
		io.NopCloser(bytes.NewReader(stream)), nopWriteCloser{io.Discard},
		nil,
		func(_ FunctionLogger, method string, _ []any) {
			mu.Lock()
			defer mu.Unlock()
			notifications = append(notifications, method)
		},
		func(err error) {
			mu.Lock()
			defer mu.Unlock()
			errs = append(errs, err)
		},
	)
	c.SetMaxIncomingMessageSize(50)

	done := make(chan struct{})
	go func() {
		c.Run()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Run did not return")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"small"}, notifications)
	require.Len(t, errs, 1)
	require.ErrorIs(t, errs[0], ErrMessageTooLarge)
}
