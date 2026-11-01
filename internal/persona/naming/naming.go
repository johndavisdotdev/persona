// Package naming generates short, readable instance names.
package naming

import (
	"crypto/rand"
	"math/big"
)

var adjectives = []string{"amber", "brisk", "calm", "deft", "eager", "fleet", "gentle", "keen", "lucid", "mellow", "nimble", "quiet", "rapid", "sage", "steady", "swift", "vivid", "witty"}
var nouns = []string{"otter", "falcon", "lynx", "heron", "fox", "badger", "wren", "orca", "moth", "ibis", "koi", "puma", "raven", "yak", "gecko", "lark"}

func pick(list []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	if err != nil {
		return list[0]
	}
	return list[n.Int64()]
}

// Random returns a name like "swift-otter-42".
func Random() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(100))
	return pick(adjectives) + "-" + pick(nouns) + "-" + n.String()
}
