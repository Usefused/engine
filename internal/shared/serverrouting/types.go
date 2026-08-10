package serverrouting

type Variable struct {
	Name     string   `json:"name"`
	Default  *string  `json:"default,omitempty"`
	Enum     []string `json:"enum,omitempty"`
	Required bool     `json:"required"`
}
