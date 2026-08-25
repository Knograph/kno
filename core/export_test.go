package core

import knov1 "github.com/knograph/kno/gen/kno/v1"

// CheckResumableForTest exposes checkResumable, which is the gate M2-10e arms
// and which no exported surface reaches.
func CheckResumableForTest(o BaselineOptions, run *knov1.Run) error {
	return o.checkResumable(run)
}
