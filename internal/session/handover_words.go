package session

import "crypto/rand"

// handoverWords is a curated list of short, distinct, easy-to-read words
// used to generate deterministic-looking handover file names.
// 215 words → 3 picks ≈ 9.9 million combinations.
var handoverWords = []string{
	"amber", "anchor", "apple", "atlas", "autumn",
	"badge", "birch", "blade", "bloom", "bolt",
	"brave", "brick", "brook", "brush", "burst",
	"cabin", "cairn", "calm", "cape", "cedar",
	"chalk", "charm", "chase", "chess", "chief",
	"cider", "claim", "clash", "cliff", "clock",
	"cloud", "clover", "coast", "coral", "crane",
	"creek", "crest", "crisp", "crown", "crystal",
	"dance", "dapple", "dawn", "delta", "dew",
	"drift", "dune", "dusk", "eagle", "echo",
	"ember", "fable", "fawn", "fern", "field",
	"finch", "flame", "flare", "flint", "flora",
	"forge", "frost", "gale", "garden", "gate",
	"gem", "glade", "gleam", "glen", "glow",
	"grace", "grain", "grove", "halo", "harbor",
	"haven", "hawk", "hazel", "heath", "hedge",
	"heron", "hill", "holly", "horizon", "husk",
	"inlet", "iron", "isle", "ivy", "jade",
	"jasper", "jewel", "kelp", "kite", "knoll",
	"lace", "lake", "lance", "lark", "laurel",
	"leaf", "light", "lily", "linen", "lodge",
	"lotus", "lunar", "maple", "marsh", "mast",
	"meadow", "mesa", "mist", "moon", "moss",
	"north", "nova", "oak", "oasis", "ocean",
	"olive", "onyx", "orbit", "otter", "palm",
	"patch", "path", "pearl", "petal", "pier",
	"pine", "plume", "point", "pond", "port",
	"prism", "pulse", "quartz", "quest", "rain",
	"range", "rapid", "raven", "reef", "ridge",
	"river", "robin", "rock", "root", "rose",
	"rowan", "ruby", "sage", "sail", "sand",
	"scone", "scout", "shade", "shell", "shore",
	"silk", "silver", "slate", "snow", "solar",
	"spark", "spire", "spruce", "star", "steam",
	"steel", "stem", "stone", "storm", "stream",
	"summit", "sun", "swift", "thorn", "thyme",
	"tide", "timber", "torch", "trail", "tulip",
	"vale", "vapor", "velvet", "vine", "violet",
	"wave", "wheat", "willow", "wind", "wren",
	"yarrow", "yew", "zeal", "zenith", "zinc",
}

// RandomHandoverSlug returns a string like "amber-creek-bloom" using
// crypto/rand for word selection.
func RandomHandoverSlug() string {
	w1 := handoverWords[randInt(len(handoverWords))]
	w2 := handoverWords[randInt(len(handoverWords))]
	w3 := handoverWords[randInt(len(handoverWords))]
	return w1 + "-" + w2 + "-" + w3
}

func randInt(max int) int {
	var b [1]byte
	_, _ = rand.Read(b[:])
	return int(b[0]) % max
}
