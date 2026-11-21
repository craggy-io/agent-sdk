package pricesheet_test

import (
	"context"
	"fmt"
	"time"

	"github.com/craggy-io/agent-sdk/pricesheet"
)

func Example() {
	// A host keeps one Reader and asks it for the sheet when it needs a model.
	// Here nothing answers, so the built-in sheet is what it holds.
	r := &pricesheet.Reader{URL: "http://127.0.0.1:1/v1/sheet.json", Timeout: time.Second}
	sheet := r.Current(context.Background())

	m := sheet.Tier(pricesheet.Low)
	fmt.Println(m.ID, r.Status().Source)
	fmt.Printf("$%.2f in, $%.2f out per million tokens\n", m.InUSDPerM, m.OutUSDPerM)
	// Output:
	// meta/muse-spark-1.3-contributor built-in
	// $0.10 in, $0.20 out per million tokens
}
