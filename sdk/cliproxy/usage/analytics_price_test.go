package usage

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func archivedPriceEvent(id string) AccountingEvent {
	event := exportEvent(id)
	event.Estimate.Rounding = "half_up_9_decimal_places"
	event.Estimate.Source = "litellm"
	event.Estimate.Rates = &config.PriceRate{
		Provider: "openai", Model: "m", Currency: "USD", MinContext: 500001,
		Input: priceString("0.000002500000000001"), Output: priceString("0.000015"),
	}
	return event
}

func TestAnalyticsPriceIdentityAndExactRates(t *testing.T) {
	event := archivedPriceEvent("one")
	row, err := newAnalyticsPriceRow(event.Estimate)
	if err != nil {
		t.Fatal(err)
	}
	if row.InputUSDPerMTok == nil || *row.InputUSDPerMTok != "2.500000000001" || row.ReasoningUSDPerMTok == nil || *row.ReasoningUSDPerMTok != "15" || row.MaxContextTokens != nil || row.ServiceTier != nil || len(row.PriceID) != 64 {
		t.Fatalf("incorrect price: %+v", row)
	}

	event.Estimate.Snapshot = "another-catalog"
	event.Estimate.Rates.Output = priceString("0.000015000")
	same, err := newAnalyticsPriceRow(event.Estimate)
	if err != nil || !reflect.DeepEqual(row, same) {
		t.Fatalf("equivalent rate changed identity: %+v %v", same, err)
	}
	event.Estimate.Rates.MinContext++
	changed, err := newAnalyticsPriceRow(event.Estimate)
	if err != nil || changed.PriceID == row.PriceID {
		t.Fatal("context band did not change identity")
	}
	event.Estimate.Rates.Input = priceString("0.0000025000000000001")
	if _, err := newAnalyticsPriceRow(event.Estimate); err == nil {
		t.Fatal("silently rounded an unrepresentable rate")
	}
}
