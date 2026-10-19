// Package process owns the execution and progress rendering of the
// `term-llm process` commands; cmd keeps only the Cobra definitions.
package process

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/samsaffron/term-llm/internal/process"
)

// ListFunc returns the validated local process records.
type ListFunc func() ([]process.Record, error)

// List writes the registered processes other than self, sorted by PID.
func List(out io.Writer, list ListFunc, self int, asJSON bool) error {
	records, err := list()
	if err != nil {
		return err
	}
	visible := make([]process.Record, 0, len(records))
	for _, record := range records {
		if record.PID != self {
			visible = append(visible, record)
		}
	}
	sort.Slice(visible, func(i, j int) bool { return visible[i].PID < visible[j].PID })
	if asJSON {
		return json.NewEncoder(out).Encode(visible)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PID\tMODE\tINSTANCE\tBUILD\tSTATE")
	for _, r := range visible {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", r.PID, r.Mode, r.Instance, r.Build, r.Phase)
	}
	return w.Flush()
}

// CompletePIDs returns shell completions ("PID\tdescription") for restartable
// processes whose PID starts with prefix. Registry errors yield no completions.
func CompletePIDs(list ListFunc, self int, prefix string) []string {
	records, err := list()
	if err != nil {
		return nil
	}
	sort.Slice(records, func(i, j int) bool { return records[i].PID < records[j].PID })
	var completions []string
	for _, record := range records {
		pid := strconv.Itoa(record.PID)
		if record.PID <= 0 || record.PID == self || !strings.HasPrefix(pid, prefix) {
			continue
		}
		// Cobra uses a tab-separated description where supported, and
		// strips it for shells that only accept the completion value.
		description := strings.Join(strings.Fields(progressText(record.Mode+" · "+record.Phase+" · "+record.Build)), " ")
		completions = append(completions, pid+"\t"+description)
	}
	return completions
}
