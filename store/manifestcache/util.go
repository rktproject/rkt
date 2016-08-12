package manifestcache

import (
	"fmt"
	"strings"
)

// blockTransform creates a path slice from the given string to use as a
// directory prefix. The string must be in hash format:
//    "sha256-abcdefgh"... -> []{"sha256", "ab"}
// Right now it just copies the default of git which is a two byte prefix. We
// will likely want to add re-sharding later.
func blockTransform(s string) []string {
	// TODO(philips): use spec/types.Hash after export typ field
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		panic(fmt.Errorf("blockTransform should never receive non-hash, got %v", s))
	}
	return []string{
		parts[0],
		parts[1][0:2],
	}
}
