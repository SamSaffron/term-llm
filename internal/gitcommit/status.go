package gitcommit

import (
	"bytes"
	"fmt"
	"strings"
)

func parseStatus(raw []byte, state *RepositoryState) ([]byte, []string, bool, error) {
	records := bytes.Split(raw, []byte{0})
	canonical := bytes.NewBuffer(nil)
	pathSet := map[string]struct{}{}
	intentToAdd := false
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if len(rec) == 0 {
			continue
		}
		if rec[0] == '#' {
			canonical.Write(rec)
			canonical.WriteByte(0)
			continue
		}
		s := string(rec)
		switch rec[0] {
		case '1':
			parts := strings.SplitN(s, " ", 9)
			if len(parts) != 9 {
				return nil, nil, false, fmt.Errorf("parse Git status ordinary record")
			}
			xy := parts[1]
			if len(xy) != 2 {
				return nil, nil, false, fmt.Errorf("parse Git status XY field")
			}
			path := parts[8]
			ch := Change{Path: path, Kind: kindFromCode(xy[0]), Staged: xy[0] != '.', Unstaged: xy[1] != '.', PartiallyStaged: xy[0] != '.' && xy[1] != '.', Submodule: parts[2] != "N..."}
			if xy[0] == '.' {
				ch.Kind = kindFromCode(xy[1])
			}
			if xy[0] == '.' && xy[1] == 'A' && allZero(parts[7]) {
				intentToAdd = true
			}
			appendTracked(state, ch)
			pathSet[path] = struct{}{}
			canonical.Write(rec)
			canonical.WriteByte(0)
		case '2':
			parts := strings.SplitN(s, " ", 10)
			if len(parts) != 10 || i+1 >= len(records) {
				return nil, nil, false, fmt.Errorf("parse Git status rename record")
			}
			i++
			old := string(records[i])
			xy := parts[1]
			if len(xy) != 2 {
				return nil, nil, false, fmt.Errorf("parse Git status rename XY field")
			}
			kind := ChangeRenamed
			if strings.HasPrefix(parts[8], "C") {
				kind = ChangeCopied
			}
			ch := Change{Path: parts[9], OldPath: old, Kind: kind, Staged: xy[0] != '.', Unstaged: xy[1] != '.', PartiallyStaged: xy[0] != '.' && xy[1] != '.', Submodule: parts[2] != "N..."}
			appendTracked(state, ch)
			pathSet[ch.Path] = struct{}{}
			pathSet[old] = struct{}{}
			canonical.Write(rec)
			canonical.WriteByte(0)
			canonical.Write(records[i])
			canonical.WriteByte(0)
		case 'u':
			parts := strings.SplitN(s, " ", 11)
			if len(parts) != 11 {
				return nil, nil, false, fmt.Errorf("parse Git status conflict record")
			}
			path := parts[10]
			state.Conflicted = append(state.Conflicted, Change{Path: path, Kind: ChangeConflicted, Staged: true, Unstaged: true})
			pathSet[path] = struct{}{}
			canonical.Write(rec)
			canonical.WriteByte(0)
		case '?':
			path := strings.TrimPrefix(s, "? ")
			state.Untracked = append(state.Untracked, Change{Path: path, Kind: ChangeUntracked, Untracked: true, Unstaged: true})
			pathSet[path] = struct{}{}
			canonical.Write(rec)
			canonical.WriteByte(0)
		default:
			return nil, nil, false, fmt.Errorf("parse unsupported Git status record %q", rec[0])
		}
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sortStrings(paths)
	return canonical.Bytes(), paths, intentToAdd, nil
}

func appendTracked(state *RepositoryState, ch Change) {
	if ch.Staged {
		state.Staged = append(state.Staged, ch)
	}
	if ch.Unstaged {
		state.Unstaged = append(state.Unstaged, ch)
	}
}
func kindFromCode(c byte) ChangeKind {
	switch c {
	case 'A':
		return ChangeAdded
	case 'D':
		return ChangeDeleted
	case 'R':
		return ChangeRenamed
	case 'C':
		return ChangeCopied
	case 'T':
		return ChangeTypeChanged
	default:
		return ChangeModified
	}
}
func allZero(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c != '0' {
			return false
		}
	}
	return true
}
func sortStrings(v []string) { // bytewise Git-compatible stable display ordering
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
