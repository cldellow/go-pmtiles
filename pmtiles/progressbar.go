package pmtiles

import (
	"fmt"
	"github.com/schollz/progressbar/v3"
	"log"
	"time"
)

// ShowProgress turns progress bars on or off. Logging is unaffected.
var ShowProgress = true

// progressbarOptions are the options both kinds of bar share.
func progressbarOptions(log *log.Logger, desc string) []progressbar.Option {
	opts := []progressbar.Option{
		progressbar.OptionSetDescription(desc),
		progressbar.OptionSetWriter(log.Writer()),
		progressbar.OptionSetWidth(10),
		progressbar.OptionThrottle(65 * time.Millisecond),
		progressbar.OptionShowCount(),
		progressbar.OptionSpinnerType(14),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetVisibility(ShowProgress),
	}
	if ShowProgress {
		opts = append(opts,
			progressbar.OptionOnCompletion(func() {
				fmt.Fprint(log.Writer(), "\n")
			}),
			progressbar.OptionSetRenderBlankState(true),
		)
	}
	return opts
}

func defaultProgressbar(log *log.Logger, max int64, description ...string) *progressbar.ProgressBar {
	desc := ""
	if len(description) > 0 {
		desc = description[0]
	}
	return progressbar.NewOptions64(max, append(progressbarOptions(log, desc), progressbar.OptionShowIts())...)
}

func defaultBytesProgressbar(log *log.Logger, maxBytes int64, description ...string) *progressbar.ProgressBar {
	desc := ""
	if len(description) > 0 {
		desc = description[0]
	}
	return progressbar.NewOptions64(maxBytes, append(progressbarOptions(log, desc), progressbar.OptionShowBytes(true))...)
}
