package cmd

import (
	"context"
	"sort"
	"strings"

	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/spf13/cobra"
)

func memoryAgentCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	store, err := openMemoryStore()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	defer store.Close()

	agents, err := store.ListAgents(context.Background())
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	var completions []string
	for _, agent := range agents {
		if strings.HasPrefix(agent, toComplete) {
			completions = append(completions, agent)
		}
	}

	return completions, cobra.ShellCompDirectiveNoFileComp
}

func memoryFragmentPathCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	store, err := openMemoryStore()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	defer store.Close()

	agent := ""
	if flag := cmd.Flags().Lookup("agent"); flag != nil {
		agent = flag.Value.String()
	} else if flag := cmd.InheritedFlags().Lookup("agent"); flag != nil {
		agent = flag.Value.String()
	}
	agent = strings.TrimSpace(agent)

	fragments, err := store.ListFragments(context.Background(), memorydb.ListOptions{Agent: agent})
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	seen := make(map[string]struct{})
	var completions []string
	for _, fragment := range fragments {
		if strings.HasPrefix(fragment.Path, toComplete) {
			if _, ok := seen[fragment.Path]; ok {
				continue
			}
			seen[fragment.Path] = struct{}{}
			completions = append(completions, fragment.Path)
		}
	}

	sort.Strings(completions)
	return completions, cobra.ShellCompDirectiveNoFileComp
}
