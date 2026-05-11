// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package api

import (
	"errors"
	"io"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

// =============================================================================
// extractCloseCode — handles every close code in the gorilla/websocket spec
// =============================================================================

func TestExtractCloseCode_NilErrIsUnknown(t *testing.T) {
	assert.Equal(t, "unknown", extractCloseCode(nil))
}

func TestExtractCloseCode_PlainErrIsUnknown(t *testing.T) {
	assert.Equal(t, "unknown", extractCloseCode(errors.New("connection reset")))
	assert.Equal(t, "unknown", extractCloseCode(io.EOF))
}

func TestExtractCloseCode_AbnormalClosure(t *testing.T) {
	// The smoking-gun pattern from #203 forensics.
	err := &websocket.CloseError{Code: websocket.CloseAbnormalClosure}
	assert.Equal(t, "1006", extractCloseCode(err))
}

func TestExtractCloseCode_AllSpecCodes(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{websocket.CloseNormalClosure, "1000"},
		{websocket.CloseGoingAway, "1001"},
		{websocket.CloseProtocolError, "1002"},
		{websocket.CloseUnsupportedData, "1003"},
		{websocket.CloseNoStatusReceived, "1005"},
		{websocket.CloseAbnormalClosure, "1006"},
		{websocket.CloseInvalidFramePayloadData, "1007"},
		{websocket.ClosePolicyViolation, "1008"},
		{websocket.CloseMessageTooBig, "1009"},
		{websocket.CloseMandatoryExtension, "1010"},
		{websocket.CloseInternalServerErr, "1011"},
		{websocket.CloseServiceRestart, "1012"},
		{websocket.CloseTryAgainLater, "1013"},
		{websocket.CloseTLSHandshake, "1015"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			err := &websocket.CloseError{Code: tc.code}
			assert.Equal(t, tc.want, extractCloseCode(err))
		})
	}
}

func TestExtractCloseCode_UnknownCode(t *testing.T) {
	err := &websocket.CloseError{Code: 9999}
	assert.Equal(t, "unknown", extractCloseCode(err))
}

func TestExtractCloseCode_WrappedError(t *testing.T) {
	// errors.As must traverse wrapping (e.g. fmt.Errorf("%w") around
	// a *websocket.CloseError).
	inner := &websocket.CloseError{Code: websocket.CloseAbnormalClosure}
	wrapped := errors.Join(errors.New("read failed"), inner)
	assert.Equal(t, "1006", extractCloseCode(wrapped))
}

// =============================================================================
// normalizeHostnamePrefix — bounds cardinality across known fleets
// =============================================================================

func TestIsGCPInstanceSuffix(t *testing.T) {
	// valid GCP suffixes: exactly 4 lowercase alphanumeric chars
	assert.True(t, isGCPInstanceSuffix("lcfl"))
	assert.True(t, isGCPInstanceSuffix("3lxd"))
	assert.True(t, isGCPInstanceSuffix("0000"))
	// invalid: wrong length
	assert.False(t, isGCPInstanceSuffix("abc"))
	assert.False(t, isGCPInstanceSuffix("abcde"))
	assert.False(t, isGCPInstanceSuffix(""))
	// invalid: non-alphanumeric char in a 4-char string
	assert.False(t, isGCPInstanceSuffix("ab-c"))
	assert.False(t, isGCPInstanceSuffix("AB12"))
}

func TestNormalizeHostnamePrefix(t *testing.T) {
	cases := map[string]string{
		// Real journalctl samples (2026-05-10):
		"ci-spot-us-eas-c9mp.us-east1-d.c.ci-runners-de.internal":    "ci-spot-us-eas",
		"ci-od-us-eas-4n6g.us-east1-c.c.ci-runners-de.internal":      "ci-od-us-eas",
		"ci-spot-us-cen-xdm8.us-central1-b.c.ci-runners-de.internal": "ci-spot-us-cen",
		"ci-od-us-cen-7zkl.us-central1-a.c.ci-runners-de.internal":   "ci-od-us-cen",
		"ci-spot-us-wes-2abc.us-west1-b.c.ci-runners-de.internal":    "ci-spot-us-wes",
		// All-letter GCP suffixes — previous digit-check leaked these (#214):
		"ci-od-us-eas-lcfl.us-east1-b.c.ci-runners-de.internal":   "ci-od-us-eas",
		"ci-od-us-wes-wmmb.us-west1-a.c.ci-runners-de.internal":   "ci-od-us-wes",
		"ci-od-us-wes-rvcr.us-west1-b.c.ci-runners-de.internal":   "ci-od-us-wes",
		"ci-spot-us-eas-rbdg.us-east1-c.c.ci-runners-de.internal": "ci-spot-us-eas",
		// No region segment — surface what we have, capped.
		"integration-test-vm":         "integration-test-vm",
		"d3ci42":                      "d3ci42",
		"":                            "unknown",
		"localhost":                   "localhost",
		"some.fully.qualified.domain": "some",
	}
	for hostname, want := range cases {
		t.Run(hostname, func(t *testing.T) {
			assert.Equal(t, want, normalizeHostnamePrefix(hostname))
		})
	}
}

func TestNormalizeHostnamePrefix_LongHostnameTruncated(t *testing.T) {
	// Pathological input — a 50-char unstructured hostname must not
	// blow the cardinality budget.
	long := "this-is-a-very-long-hostname-that-runs-on-and-on"
	got := normalizeHostnamePrefix(long)
	assert.LessOrEqual(t, len(got), 32, "hostname prefix must be capped to 32 chars")
}

// =============================================================================
// recordWSClose — end-to-end increment on the labelled counter
// =============================================================================

func TestRecordWSClose_IncrementsByLabel(t *testing.T) {
	hostname := "ci-spot-us-eas-xyzq.us-east1-d.c.ci-runners-de.internal"
	before := testutil.ToFloat64(wsCloseTotal.WithLabelValues("1006", "ci-spot-us-eas"))
	recordWSClose(&websocket.CloseError{Code: websocket.CloseAbnormalClosure}, hostname)
	after := testutil.ToFloat64(wsCloseTotal.WithLabelValues("1006", "ci-spot-us-eas"))
	assert.InDelta(t, before+1, after, 0.0001)
}

func TestRecordWSClose_NonWebSocketErrTaggedUnknown(t *testing.T) {
	before := testutil.ToFloat64(wsCloseTotal.WithLabelValues("unknown", "unknown"))
	recordWSClose(errors.New("connection reset by peer"), "")
	after := testutil.ToFloat64(wsCloseTotal.WithLabelValues("unknown", "unknown"))
	assert.InDelta(t, before+1, after, 0.0001)
}
