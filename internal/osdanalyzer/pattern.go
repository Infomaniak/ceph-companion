package osdanalyzer

// AnalyzerPattern represents a YAML pattern definition
type AnalyzerPattern struct {
	Name     string   `yaml:"name"`
	Source   string   `yaml:"source"`
	Patterns []string `yaml:"patterns"`
	Findings []string `yaml:"findings"`
}

// FoundValues represents values found during pattern evaluation
type FoundValues map[string]interface{}

// EvalPatternResult represents the result of evaluating a single pattern
type EvalPatternResult struct {
	Success     bool
	Message     string
	FoundValues FoundValues
	Pattern     string
	SourceType  string
}
