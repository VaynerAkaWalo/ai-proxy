package usage

// NewLiteLLMPriceBook prices historical usage from a LiteLLM catalog the way the deployed proxy
// does, which has no local overrides or aliases.
func NewLiteLLMPriceBook(catalog []byte) (*PriceBook, error) {
	rates, err := importLiteLLM(catalog)
	if err != nil {
		return nil, err
	}

	return NewPriceBook(rates, nil, makeSources(len(rates), "litellm"))
}

// IDVPriceRecord returns the llm_usage prices record an estimate used and its identifier.
// The record is nil when the estimate selected no USD price.
func IDVPriceRecord(estimate *CostEstimate) (any, string, error) {
	row, err := newIDVPriceRow(estimate)
	if err != nil || row == nil {
		return nil, "", err
	}

	return row, row.PriceID, nil
}
