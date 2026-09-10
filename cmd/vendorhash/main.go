// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"os"

	"github.com/kradalby/tsnixcache/flakehash"
)

func main() { os.Exit(flakehash.Main(os.Args[1:])) }
