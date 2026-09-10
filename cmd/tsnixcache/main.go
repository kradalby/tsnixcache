// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"os"

	"github.com/kradalby/tsnixcache/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
