package runtime

import "strings"

func ExtractAgentFromArgs(args []string) (agentName string, filteredArgs []string) {
	for _, arg := range args {
		if strings.HasPrefix(arg, "@") && len(arg) > 1 {
			agentName = arg[1:] // strip the @
		} else {
			filteredArgs = append(filteredArgs, arg)
		}
	}
	return agentName, filteredArgs
}
