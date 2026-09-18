package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/terminaltext"
	"github.com/samsaffron/term-llm/internal/typesafe"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const maxClassifyInputBytes = 16 << 20

type classifyClient interface {
	Classify(context.Context, typesafe.Request) (*typesafe.Response, error)
	ListModels(context.Context) (*typesafe.ModelsResponse, error)
}

type classifyDeps struct {
	loadConfig func() (*config.Config, error)
	newClient  func(typesafe.Options) (classifyClient, error)
	stdinData  func(*cobra.Command) bool
}

type classifyOptions struct {
	provider         string
	stateFile        string
	stateJSON        bool
	questionsFile    string
	sugarType        string
	sugarQuestion    string
	sugarName        string
	sugarOptions     []string
	sugarLevels      []string
	trueDescription  string
	falseDescription string
	model            string
	baseURL          string
	timeout          time.Duration
	output           string
	format           string
	prettyPrint      bool
	answer           string
}

func init() {
	rootCmd.AddCommand(newClassifyCmd(classifyDeps{}))
}

func newClassifyCmd(deps classifyDeps) *cobra.Command {
	if deps.loadConfig == nil {
		deps.loadConfig = config.Load
	}
	if deps.newClient == nil {
		deps.newClient = func(opts typesafe.Options) (classifyClient, error) { return typesafe.NewClient(opts) }
	}
	if deps.stdinData == nil {
		deps.stdinData = defaultClassifyStdinData
	}
	opts := &classifyOptions{sugarName: "result", format: "json"}
	cmd := &cobra.Command{
		Use:   "classify [state]",
		Short: "Classify state with a classification provider",
		Long: `Evaluate text or structured JSON with a classification provider (TypeSafe System One).

Select classify.default_provider from classify.providers, or override it with --provider/-p.

Questions can be supplied together in a JSON/YAML file, or defined inline for
a single choice, score, or noul. State and questions are sent to the configured endpoint. TypeSafe may also
be used for automatic approvals when guardian.backend is set to classify.`,
		Example: `  term-llm classify "Production is down" --type noul --question "Is this urgent?" --format value
  term-llm classify "Sam is eating ice cream" --type choice --question "Am I happy?" --option yes --option no --pretty-print
  term-llm classify "My invoice is wrong" -q routing.yaml --format table
  term-llm classify --state-json -f event.json -q checks.yaml
  term-llm classify models`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClassify(cmd, args, opts, deps)
		},
	}
	addClassifyFlags(cmd, opts)
	modelsOpts := &classifyOptions{format: "table"}
	modelsCmd := &cobra.Command{
		Use:   "models",
		Short: "List models for the selected classification provider",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClassifyModels(cmd, modelsOpts, deps)
		},
	}
	addClassifyConnectionOutputFlags(modelsCmd, modelsOpts, false)
	cmd.AddCommand(modelsCmd)
	return cmd
}

func addClassifyFlags(cmd *cobra.Command, opts *classifyOptions) {
	cmd.Flags().StringVarP(&opts.stateFile, "file", "f", "", "Read state from file ('-' for stdin)")
	cmd.Flags().BoolVar(&opts.stateJSON, "state-json", false, "Treat state input as JSON instead of a JSON string")
	cmd.Flags().StringVarP(&opts.questionsFile, "questions", "q", "", "Questions JSON/YAML file ('-' for stdin when state is not stdin)")
	cmd.Flags().StringVar(&opts.sugarType, "type", "", "Single question type: choice, score, or noul")
	cmd.Flags().StringVar(&opts.sugarQuestion, "question", "", "Single question instructions")
	cmd.Flags().StringVar(&opts.sugarName, "name", "result", "Single question answer id")
	cmd.Flags().StringArrayVar(&opts.sugarOptions, "option", nil, "Choice option key[=description] (repeatable)")
	cmd.Flags().StringArrayVar(&opts.sugarLevels, "level", nil, "Score level (repeatable)")
	cmd.Flags().StringVar(&opts.trueDescription, "true-description", "", "NOUL true description")
	cmd.Flags().StringVar(&opts.falseDescription, "false-description", "", "NOUL false description")
	cmd.Flags().StringVar(&opts.answer, "answer", "", "Answer id for --format value")
	registerClassifyCompletion(cmd, "type", classifyStaticCompletion(classifyQuestionTypes))
	registerClassifyCompletion(cmd, "answer", classifyAnswerCompletion)
	// Free-text flags: suggest nothing instead of unrelated file names.
	for _, name := range []string{"question", "name", "option", "level", "true-description", "false-description"} {
		registerClassifyCompletion(cmd, name, classifyNoCompletion)
	}
	addClassifyConnectionOutputFlags(cmd, opts, true)
}

func addClassifyConnectionOutputFlags(cmd *cobra.Command, opts *classifyOptions, valueFormat bool) {
	cmd.Flags().StringVarP(&opts.provider, "provider", "p", "", "Classification provider (defaults to classify.default_provider)")
	registerClassifyCompletion(cmd, "provider", classifyProviderCompletion)
	if valueFormat {
		cmd.Flags().StringVar(&opts.model, "model", "", "TypeSafe model (defaults to selected provider model)")
		registerClassifyCompletion(cmd, "model", classifyModelCompletion)
	}
	cmd.Flags().StringVar(&opts.baseURL, "base-url", "", "TypeSafe API base URL (defaults to selected provider base_url)")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 0, "TypeSafe request timeout (defaults to selected provider timeout_seconds)")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "", "Write output to file instead of stdout")
	cmd.Flags().BoolVar(&opts.prettyPrint, "pretty-print", false, "Indent JSON output (requires --format json)")
	registerClassifyCompletion(cmd, "base-url", classifyBaseURLCompletion)
	registerClassifyCompletion(cmd, "timeout", classifyNoCompletion)
	if valueFormat {
		cmd.Flags().StringVar(&opts.format, "format", opts.format, "Output format: json, table, or value")
		registerClassifyCompletion(cmd, "format", classifyStaticCompletion(classifyFormats))
	} else {
		cmd.Flags().StringVar(&opts.format, "format", opts.format, "Output format: json or table")
		registerClassifyCompletion(cmd, "format", classifyStaticCompletion(classifyModelsFormats))
	}
}

func runClassify(cmd *cobra.Command, args []string, opts *classifyOptions, deps classifyDeps) error {
	if err := validateClassifyFormat(opts.format, true); err != nil {
		return err
	}
	if err := validateClassifyEarlyFlags(cmd, opts); err != nil {
		return err
	}
	state, stateFromStdin, err := classifyState(cmd, args, opts, deps)
	if err != nil {
		return err
	}
	questions, err := classifyQuestions(cmd, opts, stateFromStdin)
	if err != nil {
		return err
	}
	if opts.answer != "" {
		if _, ok := questions[opts.answer]; !ok {
			return fmt.Errorf("--answer %q is not one of the requested question ids", opts.answer)
		}
	}
	if opts.format == "value" && opts.answer == "" {
		if len(questions) != 1 {
			return errors.New("--format value requires --answer when multiple questions are requested")
		}
		// Select the requested answer, not any extra answers added by the API.
		for id := range questions {
			opts.answer = id
		}
	}

	cfg, err := deps.loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	provider, err := cfg.Classify.ResolveProvider(opts.provider)
	if err != nil {
		return err
	}
	model := strings.TrimSpace(opts.model)
	if model == "" {
		model = strings.TrimSpace(provider.Model)
	}
	req := typesafe.Request{State: state, Model: model, Questions: questions}
	client, err := newTypeSafeClient(cfg, opts, deps)
	if err != nil {
		return err
	}
	resp, err := client.Classify(cmd.Context(), req)
	if err != nil {
		return err
	}
	out, err := formatClassifyResponse(resp, opts)
	if err != nil {
		return err
	}
	return writeClassifyOutput(cmd, opts.output, out)
}

func runClassifyModels(cmd *cobra.Command, opts *classifyOptions, deps classifyDeps) error {
	if err := validateClassifyFormat(opts.format, false); err != nil {
		return err
	}
	if err := validateClassifyEarlyFlags(cmd, opts); err != nil {
		return err
	}
	cfg, err := deps.loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	client, err := newTypeSafeClient(cfg, opts, deps)
	if err != nil {
		return err
	}
	resp, err := client.ListModels(cmd.Context())
	if err != nil {
		return err
	}
	out, err := formatClassifyModels(resp, opts.format, opts.prettyPrint)
	if err != nil {
		return err
	}
	return writeClassifyOutput(cmd, opts.output, out)
}

func newTypeSafeClient(cfg *config.Config, opts *classifyOptions, deps classifyDeps) (classifyClient, error) {
	provider, err := cfg.Classify.ResolveProvider(opts.provider)
	if err != nil {
		return nil, err
	}
	apiKey, err := provider.Key().Resolve()
	if err != nil {
		return nil, fmt.Errorf("resolve classify provider API key: %w", err)
	}
	baseURL := strings.TrimSpace(opts.baseURL)
	if baseURL == "" {
		baseURL, err = provider.BaseURLRef().Resolve()
		if err != nil {
			return nil, fmt.Errorf("resolve classify provider base URL: %w", err)
		}
	}
	timeout := opts.timeout
	if timeout == 0 && provider.TimeoutSeconds != 0 {
		if provider.TimeoutSeconds < 0 {
			return nil, errors.New("classify provider timeout_seconds must not be negative")
		}
		const maxTimeoutSeconds = int64(1<<63-1) / int64(time.Second)
		if int64(provider.TimeoutSeconds) > maxTimeoutSeconds {
			return nil, errors.New("classify provider timeout_seconds is too large")
		}
		timeout = time.Duration(provider.TimeoutSeconds) * time.Second
	}
	return deps.newClient(typesafe.Options{APIKey: apiKey, BaseURL: baseURL, Timeout: timeout})
}

func validateClassifyEarlyFlags(cmd *cobra.Command, opts *classifyOptions) error {
	if cmd.Flags().Changed("timeout") && opts.timeout <= 0 {
		return errors.New("--timeout must be greater than 0")
	}
	if cmd.Flags().Changed("model") && strings.TrimSpace(opts.model) == "" {
		return errors.New("--model must not be empty")
	}
	if cmd.Flags().Changed("base-url") && strings.TrimSpace(opts.baseURL) == "" {
		return errors.New("--base-url must not be empty")
	}
	if opts.prettyPrint && opts.format != "json" {
		return errors.New("--pretty-print may only be used with --format json")
	}
	if strings.TrimSpace(opts.answer) != "" && opts.format != "value" {
		return errors.New("--answer may only be used with --format value")
	}
	return nil
}

func classifyState(cmd *cobra.Command, args []string, opts *classifyOptions, deps classifyDeps) (json.RawMessage, bool, error) {
	positional := strings.TrimSpace(strings.Join(args, " ")) != ""
	file := cmd.Flags().Changed("file")
	stdinReservedForQuestions := cmd.Flags().Changed("questions") && opts.questionsFile == "-"
	stdin := deps.stdinData(cmd) && opts.stateFile != "-" && !stdinReservedForQuestions
	var stdinBytes []byte
	if stdin {
		var err error
		stdinBytes, err = readClassifyLimited(cmd.InOrStdin())
		if err != nil {
			return nil, false, fmt.Errorf("read state: %w", err)
		}
		stdin = len(bytes.TrimSpace(stdinBytes)) > 0
	}
	sources := 0
	for _, present := range []bool{positional, file, stdin} {
		if present {
			sources++
		}
	}
	if sources == 0 {
		return nil, false, errors.New("state is required as positional text, --file, or stdin")
	}
	if sources > 1 {
		return nil, false, errors.New("state source is ambiguous; use only one of positional state, --file, or stdin")
	}
	var data []byte
	stateFromStdin := false
	var err error
	switch {
	case positional:
		data = []byte(strings.Join(args, " "))
		if len(data) > maxClassifyInputBytes {
			return nil, false, fmt.Errorf("input exceeds %d bytes", maxClassifyInputBytes)
		}
	case file:
		if opts.stateFile == "" {
			return nil, false, errors.New("--file requires a path or -")
		}
		if opts.stateFile == "-" && stdinReservedForQuestions {
			return nil, false, errors.New("--questions - cannot read stdin because state also uses stdin")
		}
		stateFromStdin = opts.stateFile == "-"
		data, err = readClassifyFileOrStdin(cmd, opts.stateFile)
	case stdin:
		stateFromStdin = true
		data = stdinBytes
	}
	if err != nil {
		return nil, false, fmt.Errorf("read state: %w", err)
	}
	state, err := encodeClassifyState(data, opts.stateJSON)
	return state, stateFromStdin, err
}

func encodeClassifyState(data []byte, stateJSON bool) (json.RawMessage, error) {
	if stateJSON {
		var compact bytes.Buffer
		if err := json.Compact(&compact, data); err != nil {
			return nil, fmt.Errorf("parse --state-json: %w", err)
		}
		return compact.Bytes(), nil
	}
	encoded, err := json.Marshal(string(data))
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func classifyQuestions(cmd *cobra.Command, opts *classifyOptions, stateFromStdin bool) (map[string]typesafe.Question, error) {
	questionsChanged := cmd.Flags().Changed("questions")
	sugarChanged := classifySugarChanged(cmd)
	if questionsChanged && sugarChanged {
		return nil, errors.New("--questions cannot be combined with single-question flags")
	}
	if questionsChanged {
		if opts.questionsFile == "" {
			return nil, errors.New("--questions requires a path or -")
		}
		if opts.questionsFile == "-" && stateFromStdin {
			return nil, errors.New("--questions - cannot read stdin because state also uses stdin")
		}
		data, err := readClassifyFileOrStdin(cmd, opts.questionsFile)
		if err != nil {
			return nil, fmt.Errorf("read questions: %w", err)
		}
		return parseQuestions(data)
	}
	if sugarChanged {
		return buildSugarQuestion(cmd, opts)
	}
	return nil, errors.New("questions are required via --questions or single-question flags")
}

func classifySugarChanged(cmd *cobra.Command) bool {
	for _, name := range []string{"type", "question", "name", "option", "level", "true-description", "false-description"} {
		if cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
}

func buildSugarQuestion(cmd *cobra.Command, opts *classifyOptions) (map[string]typesafe.Question, error) {
	if !cmd.Flags().Changed("type") || strings.TrimSpace(opts.sugarType) == "" {
		return nil, errors.New("--type is required for single-question mode")
	}
	if !cmd.Flags().Changed("question") || strings.TrimSpace(opts.sugarQuestion) == "" {
		return nil, errors.New("--question is required for single-question mode")
	}
	if strings.TrimSpace(opts.sugarName) == "" {
		return nil, errors.New("--name must not be empty")
	}
	q := typesafe.Question{Type: strings.ToLower(strings.TrimSpace(opts.sugarType))}
	instructions, _ := json.Marshal(opts.sugarQuestion)
	q.Instructions = instructions
	var err error
	switch q.Type {
	case "choice":
		err = buildSugarChoice(cmd, opts, &q)
	case "score":
		err = buildSugarScore(cmd, opts, &q)
	case "noul":
		err = buildSugarNoul(cmd, opts, &q)
	default:
		err = errors.New("--type must be one of choice, score, or noul")
	}
	if err != nil {
		return nil, err
	}
	return map[string]typesafe.Question{opts.sugarName: q}, nil
}

func buildSugarChoice(cmd *cobra.Command, opts *classifyOptions, q *typesafe.Question) error {
	if len(opts.sugarOptions) == 0 {
		return errors.New("--type choice requires at least one --option")
	}
	if cmd.Flags().Changed("level") || cmd.Flags().Changed("true-description") || cmd.Flags().Changed("false-description") {
		return errors.New("choice questions only accept --option criteria")
	}
	criteria := map[string]json.RawMessage{}
	for _, opt := range opts.sugarOptions {
		key, desc, hasDesc := strings.Cut(opt, "=")
		key = strings.TrimSpace(key)
		if key == "" {
			return errors.New("--option key must not be empty")
		}
		if _, exists := criteria[key]; exists {
			return fmt.Errorf("duplicate --option %q", key)
		}
		if hasDesc {
			criteria[key], _ = json.Marshal(strings.TrimSpace(desc))
		} else {
			criteria[key] = json.RawMessage("null")
		}
	}
	q.Criteria, _ = json.Marshal(criteria)
	return nil
}

func buildSugarScore(cmd *cobra.Command, opts *classifyOptions, q *typesafe.Question) error {
	if len(opts.sugarLevels) < 2 {
		return errors.New("--type score requires at least two --level values")
	}
	if cmd.Flags().Changed("option") || cmd.Flags().Changed("true-description") || cmd.Flags().Changed("false-description") {
		return errors.New("score questions only accept --level criteria")
	}
	q.Criteria, _ = json.Marshal(opts.sugarLevels)
	return nil
}

func buildSugarNoul(cmd *cobra.Command, opts *classifyOptions, q *typesafe.Question) error {
	if cmd.Flags().Changed("option") || cmd.Flags().Changed("level") {
		return errors.New("noul questions do not accept --option or --level")
	}
	if cmd.Flags().Changed("true-description") || cmd.Flags().Changed("false-description") {
		q.Criteria, _ = json.Marshal(map[string]string{"true": opts.trueDescription, "false": opts.falseDescription})
	}
	return nil
}

func parseQuestions(data []byte) (map[string]typesafe.Question, error) {
	node, err := decodeSingleYAMLDocument(data)
	if err != nil {
		return nil, err
	}
	converted, err := yamlNodeToJSONValue(node, 0)
	if err != nil {
		return nil, fmt.Errorf("parse questions: %w", err)
	}
	context := "decode questions"
	if top, ok := converted.(map[string]any); ok {
		if wrapped, exists := top["questions"]; exists {
			// A question ID may itself be "questions". Only unwrap when
			// its value is not a typed question.
			fields, _ := wrapped.(map[string]any)
			if _, typed := fields["type"].(string); !typed {
				if len(top) != 1 {
					return nil, errors.New("questions wrapper may only contain the questions field")
				}
				converted = wrapped
				context = "decode questions wrapper"
			}
		}
	}
	data, err = json.Marshal(converted)
	if err != nil {
		return nil, fmt.Errorf("encode questions: %w", err)
	}
	var questions map[string]typesafe.Question
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&questions); err != nil {
		return nil, fmt.Errorf("%s: %w", context, err)
	}
	if len(questions) == 0 {
		return nil, errors.New("questions file must contain a question map or {questions: ...}")
	}
	return questions, nil
}

func decodeSingleYAMLDocument(data []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse questions: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("parse questions: %w", err)
		}
		return nil, errors.New("parse questions: multiple YAML documents are not supported")
	}
	return &doc, nil
}

func yamlNodeToJSONValue(node *yaml.Node, depth int) (any, error) {
	if depth > 100 {
		return nil, errors.New("questions exceed maximum nesting depth")
	}
	if node == nil {
		return nil, nil
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil, nil
		}
		return yamlNodeToJSONValue(node.Content[0], depth+1)
	}
	switch node.Kind {
	case yaml.MappingNode:
		m := make(map[string]any, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			if keyNode.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("YAML map key %q must be a scalar", keyNode.Value)
			}
			key := keyNode.Value
			if _, exists := m[key]; exists {
				return nil, fmt.Errorf("duplicate YAML map key %q", key)
			}
			value, err := yamlNodeToJSONValue(node.Content[i+1], depth+1)
			if err != nil {
				return nil, err
			}
			m[key] = value
		}
		return m, nil
	case yaml.SequenceNode:
		items := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := yamlNodeToJSONValue(child, depth+1)
			if err != nil {
				return nil, err
			}
			items = append(items, value)
		}
		return items, nil
	case yaml.ScalarNode:
		var value any
		if err := node.Decode(&value); err != nil {
			return nil, err
		}
		return value, nil
	case yaml.AliasNode:
		return nil, errors.New("YAML aliases are not supported in question files")
	default:
		return nil, nil
	}
}

func readClassifyFileOrStdin(cmd *cobra.Command, path string) ([]byte, error) {
	if path == "-" {
		return readClassifyLimited(cmd.InOrStdin())
	}
	return readClassifyFile(path)
}

func readClassifyFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readClassifyLimited(f)
}

func readClassifyLimited(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, maxClassifyInputBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > maxClassifyInputBytes {
		return nil, fmt.Errorf("input exceeds %d bytes", maxClassifyInputBytes)
	}
	return data, nil
}

func validateClassifyFormat(format string, value bool) error {
	switch format {
	case "json", "table":
		return nil
	case "value":
		if value {
			return nil
		}
	}
	if value {
		return errors.New("--format must be json, table, or value")
	}
	return errors.New("--format must be json or table")
}

func formatClassifyResponse(resp *typesafe.Response, opts *classifyOptions) ([]byte, error) {
	switch opts.format {
	case "json":
		return formatClassifyJSON(resp.Raw, resp, opts.prettyPrint)
	case "table":
		var b strings.Builder
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ANSWER\tTYPE\tVALUE\tCONFIDENCE")
		for _, id := range sortedAnswerIDs(resp.Answers) {
			a := resp.Answers[id]
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", classifyTableCell(id), classifyTableCell(a.Type), classifyTableCell(answerValue(a)), classifyTableCell(floatValue(a.Confidence)))
		}
		tw.Flush()
		return []byte(b.String()), nil
	case "value":
		id := opts.answer
		if id == "" {
			if len(resp.Answers) != 1 {
				return nil, errors.New("--format value requires --answer when response contains multiple answers")
			}
			for k := range resp.Answers {
				id = k
			}
		}
		a, ok := resp.Answers[id]
		if !ok {
			return nil, fmt.Errorf("answer %q not found", id)
		}
		return []byte(answerValue(a) + "\n"), nil
	default:
		return nil, errors.New("unsupported format")
	}
}

func sortedAnswerIDs(m map[string]typesafe.Answer) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func answerValue(a typesafe.Answer) string {
	if a.Choice != nil {
		return *a.Choice
	}
	if a.Score != nil {
		return floatValue(a.Score)
	}
	return floatValue(a.Noul)
}

func floatValue(v *float64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%g", *v)
}

func formatClassifyModels(resp *typesafe.ModelsResponse, format string, pretty bool) ([]byte, error) {
	switch format {
	case "json":
		return formatClassifyJSON(resp.Raw, resp, pretty)
	case "table":
		var b strings.Builder
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "MODEL\tRELEASE\tDESCRIPTION")
		models := append([]typesafe.Model(nil), resp.Models...)
		sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
		for _, m := range models {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", classifyTableCell(m.Name), classifyTableCell(m.ReleaseDate), classifyTableCell(m.Description))
		}
		tw.Flush()
		return []byte(b.String()), nil
	default:
		return nil, errors.New("unsupported format")
	}
}

// formatClassifyJSON returns the provider response verbatim, indenting it only
// when --pretty-print is requested so piped output stays byte-for-byte stable.
func formatClassifyJSON(raw json.RawMessage, fallback any, pretty bool) ([]byte, error) {
	if len(raw) > 0 {
		if !pretty {
			return append(append([]byte{}, raw...), '\n'), nil
		}
		var indented bytes.Buffer
		if err := json.Indent(&indented, raw, "", "  "); err != nil {
			return nil, fmt.Errorf("pretty-print response: %w", err)
		}
		indented.WriteByte('\n')
		return indented.Bytes(), nil
	}
	return json.MarshalIndent(fallback, "", "  ")
}

func classifyTableCell(s string) string {
	clean := terminaltext.SanitizeSingleLine(s)
	if clean != s || strings.ContainsAny(s, "\n\r\t") {
		return strconv.QuoteToASCII(clean)
	}
	return clean
}

func writeClassifyOutput(cmd *cobra.Command, path string, data []byte) error {
	if path != "" {
		return os.WriteFile(path, data, 0600)
	}
	_, err := cmd.OutOrStdout().Write(data)
	return err
}

func defaultClassifyStdinData(cmd *cobra.Command) bool {
	if cmd.InOrStdin() != os.Stdin {
		return true
	}
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}

var (
	classifyQuestionTypes = []string{"choice", "noul", "score"}
	classifyFormats       = []string{"json", "table", "value"}
	classifyModelsFormats = []string{"json", "table"}
)

func registerClassifyCompletion(cmd *cobra.Command, flag string, fn func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective)) {
	if err := cmd.RegisterFlagCompletionFunc(flag, fn); err != nil {
		panic("failed to register classify " + flag + " completion: " + err.Error())
	}
}

func classifyStaticCompletion(values []string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
		return filterPrefix(values, prefix), cobra.ShellCompDirectiveNoFileComp
	}
}

func classifyNoCompletion(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveNoFileComp
}

func classifyProviderCompletion(_ *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	cfg, err := config.Load()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	return filterPrefix(cfg.Classify.ProviderNames(), prefix), cobra.ShellCompDirectiveNoFileComp
}

// classifyModelCompletion offers the models reachable without a network call:
// the selected provider's configured model plus the built-in default.
func classifyModelCompletion(cmd *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	cfg, err := config.Load()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	models := []string{config.DefaultTypeSafeModel}
	selected, _ := cmd.Flags().GetString("provider")
	if provider, err := cfg.Classify.ResolveProvider(selected); err == nil {
		if model := strings.TrimSpace(provider.Model); model != "" {
			models = append(models, model)
		}
	}
	return filterPrefix(sortedUniqueStrings(models), prefix), cobra.ShellCompDirectiveNoFileComp
}

func classifyBaseURLCompletion(cmd *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	urls := []string{config.DefaultTypeSafeBaseURL}
	if cfg, err := config.Load(); err == nil {
		selected, _ := cmd.Flags().GetString("provider")
		if provider, err := cfg.Classify.ResolveProvider(selected); err == nil {
			if base := strings.TrimSpace(provider.BaseURL); base != "" {
				urls = append(urls, base)
			}
		}
	}
	return filterPrefix(sortedUniqueStrings(urls), prefix), cobra.ShellCompDirectiveNoFileComp
}

// classifyAnswerCompletion suggests the ids that the current flags request:
// the question map from --questions, or the single --name id. Once --questions
// is present it is the only source of answer ids, because --name cannot be
// combined with it.
func classifyAnswerCompletion(cmd *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	if cmd.Flags().Changed("questions") {
		path, _ := cmd.Flags().GetString("questions")
		questions, err := readCompletionQuestions(path)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		ids := make([]string, 0, len(questions))
		for id := range questions {
			ids = append(ids, id)
		}
		return filterPrefix(sortedUniqueStrings(ids), prefix), cobra.ShellCompDirectiveNoFileComp
	}
	name, _ := cmd.Flags().GetString("name")
	if strings.TrimSpace(name) == "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return filterPrefix([]string{name}, prefix), cobra.ShellCompDirectiveNoFileComp
}

// readCompletionQuestions parses a questions file for shell completion. Unlike
// the runtime path it refuses anything that is not an ordinary readable file:
// opening a FIFO blocks until a writer appears, which would hang the user's
// shell on every <TAB>, and a character device would be read to the input cap.
func readCompletionQuestions(path string) (map[string]typesafe.Question, error) {
	path = strings.TrimSpace(path)
	if path == "" || path == "-" {
		return nil, errors.New("questions are not readable during completion")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("questions path %q is not a regular file", path)
	}
	if info.Size() > maxClassifyInputBytes {
		return nil, fmt.Errorf("questions file exceeds %d bytes", maxClassifyInputBytes)
	}
	data, err := readClassifyFile(path)
	if err != nil {
		return nil, err
	}
	return parseQuestions(data)
}

func sortedUniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}
