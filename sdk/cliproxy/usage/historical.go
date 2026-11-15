package usage

func NewLiteLLMPriceBook(catalog []byte) (*PriceBook, error) {
	rates, err := importLiteLLM(catalog)
	if err != nil {
		return nil, err
	}

	return NewPriceBook(rates, nil, makeSources(len(rates), "litellm"))
}
