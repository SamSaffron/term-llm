package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func allTasks() []Task {
	return []Task{
		fizzBuzzTask{},
		binarySearchTask{},
		jsonFormatTask{},
		concurrentCounterTask{},
		allocationTask{},
	}
}

func selectTasks(spec string) ([]Task, error) {
	tasks := allTasks()
	if strings.TrimSpace(spec) == "" || strings.EqualFold(strings.TrimSpace(spec), "all") {
		return tasks, nil
	}
	byName := make(map[string]Task, len(tasks))
	var names []string
	for _, t := range tasks {
		byName[t.Name()] = t
		names = append(names, t.Name())
	}
	var selected []Task
	for _, raw := range strings.Split(spec, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		t, ok := byName[name]
		if !ok {
			sort.Strings(names)
			return nil, fmt.Errorf("unknown task %q; available: %s", name, strings.Join(names, ", "))
		}
		selected = append(selected, t)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no tasks selected")
	}
	return selected, nil
}

type fizzBuzzTask struct{}

func (fizzBuzzTask) Name() string       { return "go_fizzbuzz" }
func (fizzBuzzTask) Language() string   { return "go" }
func (fizzBuzzTask) Difficulty() string { return "easy-correctness" }
func (fizzBuzzTask) Prompt() string {
	return `Write a complete Go source file for package main, including any imports, that defines exactly this function:

func FizzBuzz(n int) []string

For 1..n return decimal numbers, except multiples of 3 are "Fizz", multiples of 5 are "Buzz", and multiples of both are "FizzBuzz". Include an import block if you use strconv, fmt, or any other package. Do not include a main function.`
}
func (fizzBuzzTask) Score(response string, timeout time.Duration) ScoreResult {
	return scoreGoFunction(response, timeout, `
func TestGenerated(t *testing.T) {
	got := FizzBuzz(16)
	want := []string{"1","2","Fizz","4","Buzz","Fizz","7","8","Fizz","Buzz","11","Fizz","13","14","FizzBuzz","16"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FizzBuzz(16) = %#v, want %#v", got, want)
	}
	if len(FizzBuzz(0)) != 0 {
		t.Fatalf("FizzBuzz(0) should be empty")
	}
}
`, "reflect")
}

type binarySearchTask struct{}

func (binarySearchTask) Name() string       { return "go_binary_search" }
func (binarySearchTask) Language() string   { return "go" }
func (binarySearchTask) Difficulty() string { return "medium-correctness" }
func (binarySearchTask) Prompt() string {
	return `Write a complete Go source file for package main, including any imports, that defines exactly this function:

func BinarySearch(xs []int, target int) int

Return the index of target in the sorted slice, or -1 when absent. It must handle empty slices, one-element slices, negative numbers, and large slices without recursion. Do not include a main function.`
}
func (binarySearchTask) Score(response string, timeout time.Duration) ScoreResult {
	return scoreGoFunction(response, timeout, `
func TestGenerated(t *testing.T) {
	cases := []struct{
		xs []int
		target int
		want int
	}{
		{[]int{}, 1, -1},
		{[]int{7}, 7, 0},
		{[]int{7}, 8, -1},
		{[]int{-9,-4,0,3,8,12}, -4, 1},
		{[]int{-9,-4,0,3,8,12}, 12, 5},
		{[]int{-9,-4,0,3,8,12}, 2, -1},
	}
	for _, c := range cases {
		if got := BinarySearch(c.xs, c.target); got != c.want {
			t.Fatalf("BinarySearch(%v, %d) = %d, want %d", c.xs, c.target, got, c.want)
		}
	}
}
`)
}

type jsonFormatTask struct{}

func (jsonFormatTask) Name() string       { return "go_json_format" }
func (jsonFormatTask) Language() string   { return "go" }
func (jsonFormatTask) Difficulty() string { return "medium-api" }
func (jsonFormatTask) Prompt() string {
	return `Write a complete Go source file for package main, including any imports, that defines exactly this function:

func FormatPersonJSON(input string) (string, error)

The input is a JSON object with fields "name" (string) and "age" (integer). Return "Name: <name>, Age: <age>". Return an error for invalid JSON, missing name, or negative age. Do not include a main function.`
}
func (jsonFormatTask) Score(response string, timeout time.Duration) ScoreResult {
	return scoreGoFunction(response, timeout, `
func TestGenerated(t *testing.T) {
	got, err := FormatPersonJSON(`+"`"+`{"name":"Ada","age":37}`+"`"+`)
	if err != nil || got != "Name: Ada, Age: 37" {
		t.Fatalf("valid person = %q, %v", got, err)
	}
	got, err = FormatPersonJSON(`+"`"+` { "age": 5, "name": "Grace" } `+"`"+`)
	if err != nil || got != "Name: Grace, Age: 5" {
		t.Fatalf("valid reordered person = %q, %v", got, err)
	}
	bad := []string{`+"`"+`not json`+"`"+`, `+"`"+`{"name":"Ada","age":-1}`+"`"+`, `+"`"+`{"age":10}`+"`"+`}
	for _, input := range bad {
		if _, err := FormatPersonJSON(input); err == nil {
			t.Fatalf("FormatPersonJSON(%s) returned nil error", input)
		}
	}
}
`)
}

type concurrentCounterTask struct{}

func (concurrentCounterTask) Name() string       { return "go_concurrent_counter" }
func (concurrentCounterTask) Language() string   { return "go" }
func (concurrentCounterTask) Difficulty() string { return "hard-correctness-race" }
func (concurrentCounterTask) Prompt() string {
	return `Write a complete Go source file for package main, including any imports, that defines a concurrency-safe Counter type with exactly these methods:

type Counter struct { ... }
func (c *Counter) Inc()
func (c *Counter) Value() int64

It must be safe under concurrent use by many goroutines. Do not include a main function.`
}
func (concurrentCounterTask) Score(response string, timeout time.Duration) ScoreResult {
	return scoreGoFunctionWithRace(response, timeout, `
func TestGenerated(t *testing.T) {
	var c Counter
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.Inc()
			}
		}()
	}
	wg.Wait()
	if got := c.Value(); got != 100000 {
		t.Fatalf("Value() = %d, want 100000", got)
	}
}
`, "sync")
}

type allocationTask struct{}

func (allocationTask) Name() string       { return "go_dedupe_perf" }
func (allocationTask) Language() string   { return "go" }
func (allocationTask) Difficulty() string { return "perf" }
func (allocationTask) Prompt() string {
	return `Write a complete Go source file for package main, including any imports, that defines exactly this function:

func DedupeStable(xs []string) []string

Return the first occurrence of each string while preserving order. It must not mutate the input. Optimize for correctness and reasonable allocations on 10k-item slices with many duplicates. Do not include a main function.`
}
func (allocationTask) Score(response string, timeout time.Duration) ScoreResult {
	return scoreGoFunction(response, timeout, `
func TestGenerated(t *testing.T) {
	in := []string{"b","a","b","c","a","d"}
	got := DedupeStable(in)
	want := []string{"b","a","c","d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DedupeStable = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(in, []string{"b","a","b","c","a","d"}) {
		t.Fatalf("input mutated: %#v", in)
	}
}

func BenchmarkGenerated(b *testing.B) {
	xs := make([]string, 10000)
	for i := range xs {
		xs[i] = fmt.Sprintf("key-%d", i%250)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got := DedupeStable(xs)
		if len(got) != 250 {
			b.Fatalf("len=%d", len(got))
		}
	}
}
`, "fmt", "reflect")
}
