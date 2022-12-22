//go:build !noavx
// +build !noavx

package register

import (
	// Pull in avx collector.
	_ "github.com/intel/nri-resmgr/pkg/avx"
)
