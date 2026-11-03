package pairing

// Person is the display-only snapshot of the person who approved a device.
// It grants no authority and is never included in model input.
type Person struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}
