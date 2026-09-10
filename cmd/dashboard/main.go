// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"log/slog"
	"os"

	"github.com/kradalby/tsnixcache/grafana"
)

func main() {
	err := grafana.Run(os.Stdout)
	if err != nil {
		slog.Error("generate dashboard", "err", err)
		os.Exit(1)
	}
}
