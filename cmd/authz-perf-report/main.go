package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const benchmarkName = "BenchmarkAuthorizationAcceptance"

var requiredPhases = []string{
	"cold_auth",
	"cache_hit",
	"authorization_check_all",
	"graphql_preflight",
	"total_request",
}

type options struct {
	samples   int
	benchtime string
	output    string
}

type benchmarkSample struct {
	nanoseconds   float64
	databaseCalls *float64
	externalCalls *float64
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "authorization performance report:", err)
		os.Exit(1)
	}
}

func run() error {
	configuration := parseOptions()
	if err := validateOptions(configuration); err != nil {
		return err
	}
	output, err := runBenchmarks(configuration)
	if err != nil {
		return err
	}
	groups, err := parseBenchmarkOutput(output)
	if err != nil {
		return err
	}
	report := renderReport(configuration, groups)
	fmt.Print(report)
	if configuration.output != "" {
		if err := os.WriteFile(configuration.output, []byte(report), 0o644); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}
	return nil
}

func parseOptions() options {
	configuration := options{}
	flag.IntVar(&configuration.samples, "samples", 20, "independent benchmark samples")
	flag.StringVar(&configuration.benchtime, "benchtime", "100x", "Go benchmark duration or iteration count")
	flag.StringVar(&configuration.output, "output", "", "optional Markdown output path")
	flag.Parse()
	return configuration
}

func validateOptions(configuration options) error {
	if configuration.samples < 10 {
		return errors.New("samples must be at least 10 for useful tail percentiles")
	}
	if strings.TrimSpace(configuration.benchtime) == "" {
		return errors.New("benchtime must not be empty")
	}
	if os.Getenv("DATABASE_URL") == "" {
		return errors.New("DATABASE_URL is required and must point to an isolated benchmark database")
	}
	if os.Getenv("FUSED_BENCHMARK_ALLOW_DB_RESET") != "1" {
		return errors.New("set FUSED_BENCHMARK_ALLOW_DB_RESET=1 to acknowledge the isolated database reset")
	}
	return nil
}

func runBenchmarks(configuration options) ([]byte, error) {
	arguments := []string{
		"test",
		"./internal/engine/store",
		"./internal/engine/api",
		"-run", "^$",
		"-bench", "^" + benchmarkName + "$",
		"-benchmem",
		"-count", strconv.Itoa(configuration.samples),
		"-benchtime", configuration.benchtime,
	}
	command := exec.Command("go", arguments...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("run benchmarks: %w\n%s\n%s", err, string(output), stderr.String())
	}
	return output, nil
}

func parseBenchmarkOutput(output []byte) (map[string][]benchmarkSample, error) {
	groups := make(map[string][]benchmarkSample)
	for _, line := range strings.Split(string(output), "\n") {
		name, sample, ok := parseBenchmarkLine(line)
		if ok {
			groups[name] = append(groups[name], sample)
		}
	}
	if err := validatePhases(groups); err != nil {
		return nil, err
	}
	return groups, nil
}

func parseBenchmarkLine(line string) (string, benchmarkSample, bool) {
	fields := strings.Fields(line)
	if len(fields) < 4 || !strings.HasPrefix(fields[0], benchmarkName+"/") {
		return "", benchmarkSample{}, false
	}
	nanoseconds, ok := metricValue(fields, "ns/op")
	if !ok {
		return "", benchmarkSample{}, false
	}
	sample := benchmarkSample{nanoseconds: nanoseconds}
	if value, found := metricValue(fields, "db_queries/op"); found {
		sample.databaseCalls = &value
	}
	if value, found := metricValue(fields, "external_queries/op"); found {
		sample.externalCalls = &value
	}
	return trimCPUCount(fields[0]), sample, true
}

func metricValue(fields []string, unit string) (float64, bool) {
	for index := 1; index < len(fields); index++ {
		if fields[index] != unit || index == 0 {
			continue
		}
		value, err := strconv.ParseFloat(fields[index-1], 64)
		return value, err == nil
	}
	return 0, false
}

func trimCPUCount(name string) string {
	separator := strings.LastIndex(name, "-")
	if separator == -1 {
		return name
	}
	if _, err := strconv.Atoi(name[separator+1:]); err != nil {
		return name
	}
	return name[:separator]
}

func validatePhases(groups map[string][]benchmarkSample) error {
	present := make(map[string]bool)
	for name := range groups {
		present[benchmarkPhase(name)] = true
	}
	missing := make([]string, 0)
	for _, phase := range requiredPhases {
		if !present[phase] {
			missing = append(missing, phase)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("benchmark output omitted required phases: %s", strings.Join(missing, ", "))
	}
	return nil
}

func benchmarkPhase(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) < 2 {
		return "unknown"
	}
	return parts[1]
}

func renderReport(configuration options, groups map[string][]benchmarkSample) string {
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	var report strings.Builder
	report.WriteString("# Authorization performance evidence\n\n")
	fmt.Fprintf(&report, "Generated: %s  \n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&report, "Runtime: %s/%s, %s  \n", runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Fprintf(&report, "Samples: %d; benchtime: %s\n\n", configuration.samples, configuration.benchtime)
	report.WriteString("| Phase | Scenario | Samples | p50 | p95 | p99 | DB queries/op | External queries/op |\n")
	report.WriteString("|---|---|---:|---:|---:|---:|---:|---:|\n")
	for _, name := range names {
		writeReportRow(&report, name, groups[name])
	}
	return report.String()
}

func writeReportRow(report *strings.Builder, name string, samples []benchmarkSample) {
	values := make([]float64, len(samples))
	for index, sample := range samples {
		values[index] = sample.nanoseconds
	}
	sort.Float64s(values)
	phase := benchmarkPhase(name)
	scenario := strings.TrimPrefix(name, benchmarkName+"/"+phase+"/")
	fmt.Fprintf(report, "| %s | %s | %d | %s | %s | %s | %s | %s |\n",
		phase, scenario, len(samples), formatNanoseconds(percentile(values, 0.50)),
		formatNanoseconds(percentile(values, 0.95)), formatNanoseconds(percentile(values, 0.99)),
		formatOptionalMetric(samples, true), formatOptionalMetric(samples, false))
}

func percentile(sorted []float64, fraction float64) float64 {
	// Nearest-rank makes the tail value explicit and stable for small sample sets.
	index := int(math.Ceil(fraction*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	return sorted[index]
}

func formatNanoseconds(value float64) string {
	return time.Duration(math.Round(value)).String()
}

func formatOptionalMetric(samples []benchmarkSample, database bool) string {
	values := make([]float64, 0, len(samples))
	for _, sample := range samples {
		value := sample.externalCalls
		if database {
			value = sample.databaseCalls
		}
		if value != nil {
			values = append(values, *value)
		}
	}
	if len(values) == 0 {
		return "—"
	}
	return strconv.FormatFloat(mean(values), 'f', 2, 64)
}

func mean(values []float64) float64 {
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}
