package main

// allHarnesses is the complete adapter roster. Keeping registration here makes
// adding an adapter a one-file implementation plus one obvious registration
// line, without coupling the CLI parser to adapter internals.
func allHarnesses() []Harness {
	return []Harness{
		claudeHarness{},
		codexHarness{},
		piHarness{},
		geminiHarness{},
		antigravityHarness{},
		openCodeHarness{},
		crushHarness{},
	}
}
