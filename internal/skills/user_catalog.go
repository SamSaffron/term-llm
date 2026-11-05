package skills

// UserCatalogEntry describes one user-invocable skill for slash-command clients.
type UserCatalogEntry struct {
	Name                string `json:"name"`
	Description         string `json:"description"`
	ArgumentHint        string `json:"argument_hint"`
	Execution           string `json:"execution"`
	Source              string `json:"source"`
	CollidesWithBuiltin bool   `json:"collides_with_builtin"`
}

// UserCatalogDiagnostic reports a skill omitted for malformed invocation metadata.
type UserCatalogDiagnostic struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// UserCatalog lists user-invocable skills for a slash-command client. builtin
// holds the client's own command names, which win over a skill of the same name.
func (r *Registry) UserCatalog(builtin map[string]bool) ([]UserCatalogEntry, []UserCatalogDiagnostic, error) {
	catalog, diagnostics, err := r.ListUserInvocable()
	if err != nil {
		return nil, nil, err
	}
	entries := make([]UserCatalogEntry, 0, len(catalog))
	for _, skill := range catalog {
		metadata, metadataErr := InvocationFor(skill)
		if metadataErr != nil {
			continue
		}
		entries = append(entries, UserCatalogEntry{
			Name:                skill.Name,
			Description:         skill.Description,
			ArgumentHint:        metadata.ArgumentHint,
			Execution:           metadata.Execution.String(),
			Source:              skill.Source.SourceName(),
			CollidesWithBuiltin: builtin[skill.Name],
		})
	}
	problems := make([]UserCatalogDiagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		problems = append(problems, UserCatalogDiagnostic{Name: diagnostic.Name, Error: diagnostic.Err.Error()})
	}
	return entries, problems, nil
}
