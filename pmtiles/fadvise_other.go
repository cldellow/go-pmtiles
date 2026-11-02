//go:build !linux

package pmtiles

import "os"

func adviseRandom(f *os.File) {}
