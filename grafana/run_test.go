// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package grafana

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRun(t *testing.T) {
	var output bytes.Buffer
	require.NoError(t, Run(&output))

	var model struct {
		UID    string            `json:"uid"`
		Panels []json.RawMessage `json:"panels"`
	}
	require.NoError(t, json.Unmarshal(output.Bytes(), &model))
	require.Equal(t, "tsnixcache", model.UID)
	require.NotEmpty(t, model.Panels)

	reader, writer := io.Pipe()

	require.NoError(t, reader.Close())
	defer writer.Close()

	require.ErrorIs(t, Run(writer), io.ErrClosedPipe)
}
