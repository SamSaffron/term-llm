package serveui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// AssetManifest describes the content-addressed chat build. Paths are relative
// to the UI base (dist/...), never absolute, so proxied nodes share the contract.
type AssetManifest struct {
	EntryJS  string
	EntryCSS []string
	Files    []string
}

var chatManifestOnce sync.Once
var chatManifest AssetManifest
var hashedAssets map[string]bool
var hashedName = regexp.MustCompile(`-[A-Za-z0-9_-]{8,}\.[a-zA-Z0-9]+$`)

// ChatAssetManifest returns a copy of the embedded Vite manifest's public paths.
// A malformed build fails closed rather than silently serving a stable entry.
func ChatAssetManifest() AssetManifest {
	chatManifestOnce.Do(loadChatManifest)
	return AssetManifest{EntryJS: chatManifest.EntryJS, EntryCSS: append([]string(nil), chatManifest.EntryCSS...), Files: append([]string(nil), chatManifest.Files...)}
}

func loadChatManifest() {
	data, err := StaticAsset("dist/asset-manifest.json")
	if err != nil {
		panic(fmt.Sprintf("read chat asset manifest: %v", err))
	}
	var manifest map[string]struct {
		File    string   `json:"file"`
		CSS     []string `json:"css"`
		Assets  []string `json:"assets"`
		IsEntry bool     `json:"isEntry"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		panic(fmt.Sprintf("parse chat asset manifest: %v", err))
	}
	hashedAssets = make(map[string]bool)
	add := func(file string) string {
		name := "dist/" + file
		if !hashedName.MatchString(file) || strings.Contains(file, "..") || strings.HasPrefix(file, "/") {
			panic("invalid hashed chat asset: " + file)
		}
		if _, err := StaticAsset(name); err != nil {
			panic(fmt.Sprintf("missing chat asset %s: %v", name, err))
		}
		hashedAssets[name] = true
		return name
	}
	for _, entry := range manifest {
		file := add(entry.File)
		for _, asset := range entry.Assets {
			add(asset)
		}
		for _, css := range entry.CSS {
			add(css)
		}
		if entry.IsEntry {
			if chatManifest.EntryJS != "" {
				panic("multiple chat entries")
			}
			chatManifest.EntryJS = file
			for _, css := range entry.CSS {
				chatManifest.EntryCSS = append(chatManifest.EntryCSS, "dist/"+css)
			}
		}
	}
	if chatManifest.EntryJS == "" || len(chatManifest.EntryCSS) != 1 {
		panic("chat manifest must contain one JS entry and one entry stylesheet")
	}
	for file := range hashedAssets {
		chatManifest.Files = append(chatManifest.Files, file)
	}
	sort.Strings(chatManifest.Files)
}

// IsHashedAsset only trusts files declared in the embedded build manifest.
func IsHashedAsset(name string) bool {
	chatManifestOnce.Do(loadChatManifest)
	return hashedAssets[name]
}

// PrewarmAssetPaths identifies the eager shell and optional transport chunks.
func PrewarmAssetPaths(webRTC bool) []string {
	manifest := ChatAssetManifest()
	paths := append([]string{manifest.EntryJS}, manifest.EntryCSS...)
	for _, name := range manifest.Files {
		isWarmChunk := strings.HasPrefix(name, "dist/chunks/vendor-") || webRTC && strings.HasPrefix(name, "dist/chunks/webrtc-")
		if isWarmChunk && strings.HasSuffix(name, ".js") {
			paths = append(paths, name)
		}
	}
	return paths
}
