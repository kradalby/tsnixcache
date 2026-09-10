// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package grafana

import (
	"encoding/json"
	"fmt"
	"io"
)

// Run writes one validated, indented dashboard model for file provisioning.
func Run(out io.Writer) error {
	d, err := buildDashboard()
	if err != nil {
		return fmt.Errorf("build dashboard: %w", err)
	}

	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal dashboard: %w", err)
	}

	_, err = out.Write(append(data, '\n'))
	if err != nil {
		return fmt.Errorf("write dashboard: %w", err)
	}

	return nil
}
