package core

// ModelServiceTier describes an optional service tier advertised by a model catalog.
type ModelServiceTier struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}
