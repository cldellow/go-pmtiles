package pmtiles

import (
	"os"

	"golang.org/x/sys/unix"
)

// adviseRandom tells the kernel f will be read at random, so it skips readahead.
func adviseRandom(f *os.File) {
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_RANDOM)
}
