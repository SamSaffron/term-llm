package tools

import "strconv"

func buildRipgrepArgs(a GrepArgs, searchPath string, contextLines, maxResults int) []string {
	args := []string{
		"--no-config",
		"--color=never",
		"--hidden",
		"--glob", "!.git",
	}

	if a.FilesWithMatches {
		args = append(args, "--files-with-matches")
	} else {
		args = append(args,
			"--json",
			// --max-count is per-file, not global. runRipgrep also applies a
			// streaming global limit so broad searches can stop as soon as the
			// caller's max_results is satisfied.
			"--max-count", strconv.Itoa(maxResults),
			"--context", strconv.Itoa(contextLines),
		)
	}

	if a.Include != "" {
		args = append(args, "--glob", a.Include)
	}
	if a.Exclude != "" {
		args = append(args, "--glob", "!"+a.Exclude)
	}
	if a.Type != "" {
		args = append(args, "--type", a.Type)
	}
	if a.Multiline {
		args = append(args, "--multiline", "--multiline-dotall")
	}

	// Keep untrusted patterns out of ripgrep's option namespace (notably --pre).
	args = append(args, "-e", a.Pattern, "--", searchPath)
	return args
}
