package domain

// Language is the language of a script or of a news source.
type Language string

const (
	// LanguageRussian is the only supported output language of stage one.
	LanguageRussian Language = "ru"
	// LanguageEnglish is accepted for input sources.
	LanguageEnglish Language = "en"
)

// Valid reports whether the language is supported.
func (l Language) Valid() bool {
	return l == LanguageRussian || l == LanguageEnglish
}

// Validate reports whether the language is supported.
func (l Language) Validate() error {
	if !l.Valid() {
		return invalid("language")
	}
	return nil
}
