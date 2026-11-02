package usage

func NewLiteLLMPriceBook(catalog []byte) (*PriceBook, error) {
	rates, err := importLiteLLM(catalog)
	if err != nil {
		return nil, err
	}

	return NewPriceBook(rates, nil, makeSources(len(rates), "litellm"))
}

func AnalyticsPriceRecord(estimate *CostEstimate) (any, string, error) {
	row, err := newAnalyticsPriceRow(estimate)
	if err != nil || row == nil {
		return nil, "", err
	}

	return row, row.PriceID, nil
}
