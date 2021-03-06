package blob

import (
	"os"
	"syscall"
	"time"
)

func accessTime(fi os.FileInfo) time.Time {
	ts := fi.Sys().(*syscall.Stat_t).Atimespec
	return time.Unix(ts.Sec, ts.Nano())
}
