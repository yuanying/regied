// Reading an nftables counter lives outside the build tag, like reading a response does:
// it needs neither a network namespace nor privileges.

package netns

import (
	"fmt"
	"strconv"
	"strings"
)

// parseCounterPackets reads the packet count out of what `nft list counter` prints.
func parseCounterPackets(out string) (int, error) {
	fields := strings.Fields(out)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "packets" {
			n, err := strconv.Atoi(fields[i+1])
			if err != nil {
				return 0, fmt.Errorf("cannot read the packet count in %q: %w", strings.TrimSpace(out), err)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("no packet count in %q", strings.TrimSpace(out))
}
