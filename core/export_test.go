package core

import knov1 "github.com/knograph/kno/gen/kno/v1"

// CheckResumableForTest exposes checkResumable, which is the gate M2-10e arms
// and which no exported surface reaches.
func CheckResumableForTest(o BaselineOptions, run *knov1.Run) error {
	return o.checkResumable(run)
}

// ModelGateForTest exposes the resolved-model gate, which nothing exported
// reaches. The end-to-end path is covered by driving a real run; this is for
// the membership table, where a fixture Run would add nothing.
func ModelGateForTest(recorded ...string) func(now string) error {
	g := newModelGate(&knov1.Run{
		CaseExecution: &knov1.CaseExecution{ResolvedModels: recorded},
	})
	return g.check
}
