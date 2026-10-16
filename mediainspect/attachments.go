package mediainspect

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// AttachmentPayloadBytes counts each preserved attachment once. Matroska fonts
// carry their payload as codec extradata, commonly without NUMBER_OF_BYTES tags.
// Missing sizes cannot be interpreted as zero when enforcing savings limits.
func AttachmentPayloadBytes(attachments []DetailedStream) (int64, error) {
	var total int64
	for _, stream := range attachments {
		bytes := stream.ExtraDataSize
		if bytes < 0 {
			return 0, fmt.Errorf("invalid attachment extradata size for stream_%d", stream.Index)
		}
		if bytes == 0 {
			if raw := strings.TrimSpace(stream.Tags["NUMBER_OF_BYTES"]); raw != "" {
				var err error
				bytes, err = strconv.ParseInt(raw, 10, 64)
				if err != nil || bytes <= 0 {
					return 0, fmt.Errorf("invalid attachment byte count for stream_%d", stream.Index)
				}
			}
		}
		if bytes == 0 {
			return 0, fmt.Errorf("missing attachment payload size for stream_%d; savings estimate unavailable", stream.Index)
		}
		if total > math.MaxInt64-bytes {
			return 0, fmt.Errorf("attachment payload size overflow")
		}
		total += bytes
	}
	return total, nil
}
