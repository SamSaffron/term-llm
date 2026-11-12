package serve

import (
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/skills"
)

func (s *serveServer) skillsForServeSession(sess *session.Session) *skills.Setup {
	skillsConfig := s.skillsConfig
	if skillsConfig == nil && s.cfgRef != nil {
		skillsConfig = &s.cfgRef.Skills
	}
	if skillsConfig != nil && sess != nil {
		dir := firstNonEmptyString(sess.WorktreeDir, sess.CWD)
		if dir == "" {
			// An ambient process directory is not a stable shared cache identity.
			return SetupSkillsInDir(skillsConfig, "", "", io.Discard, dir)
		}
		cacheKey := filepath.Clean(dir)
		s.skillsCacheMu.Lock()
		cached := s.skillsByDir[cacheKey]
		s.skillsCacheMu.Unlock()
		if cached.setup != nil && time.Since(cached.discovered) < serveSkillsCacheTTL {
			return cached.setup
		}

		// Do not serialize cold filesystem discovery across unrelated sessions.
		setup := SetupSkillsInDir(skillsConfig, "", "", io.Discard, dir)
		if setup == nil {
			return nil
		}
		s.skillsCacheMu.Lock()
		defer s.skillsCacheMu.Unlock()
		if cached = s.skillsByDir[cacheKey]; cached.setup != nil && time.Since(cached.discovered) < serveSkillsCacheTTL {
			return cached.setup
		}
		if s.skillsByDir == nil {
			s.skillsByDir = make(map[string]serveSkillsCacheEntry)
		}
		if len(s.skillsByDir) >= 32 {
			var oldestKey string
			for existing, entry := range s.skillsByDir {
				if oldestKey == "" || entry.discovered.Before(s.skillsByDir[oldestKey].discovered) {
					oldestKey = existing
				}
			}
			delete(s.skillsByDir, oldestKey)
		}
		s.skillsByDir[cacheKey] = serveSkillsCacheEntry{setup: setup, discovered: time.Now()}
		return setup
	}
	return s.skillsSetup
}

// serveRuntimeSkills retains the effective CLI-over-agent selection on restore.
// Setups containing admission results belong to one runtime, never the catalog cache.
type serveRuntimeSkills struct {
	agentSkills string
	skillsDir   string
	skillsSetup *skills.Setup
}

func (s *serveServer) configureRuntimeSkillsForDir(rt *serveRuntime, dir string) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	s.configureRuntimeSkillsForDirLocked(rt, dir)
}

// The caller holds rt.mu (for example through lockIdleMetadataMutation).
func (s *serveServer) configureRuntimeSkillsForDirLocked(rt *serveRuntime, dir string) {
	if s == nil || rt == nil || strings.TrimSpace(dir) == "" {
		return
	}
	dir = filepath.Clean(dir)
	if rt.skillsDir != dir {
		skillsCfg := s.skillsConfig
		if s.cfgRef != nil {
			skillsCfg = &s.cfgRef.Skills
		}
		rt.skillsSetup = s.skillsSetup
		if skillsCfg != nil {
			rt.skillsSetup = SetupSkillsInDir(skillsCfg, "", rt.agentSkills, io.Discard, dir)
		}
		rt.skillsDir = dir
	}
	if rt.engine != nil {
		RegisterSkillToolWithEngine(rt.engine, rt.toolMgr, rt.skillsSetup)
	}
	configureRuntimeSkillsMetadataLocked(rt, rt.skillsSetup)
}

func configureRuntimeSkillsMetadataLocked(rt *serveRuntime, setup *skills.Setup) {
	if selected := rt.inputs.Load(); selected != nil {
		rt.systemPrompt = selected.Prompt
		return
	}
	if rt.baseSystemPrompt == "" && rt.settings == nil {
		rt.baseSystemPrompt = rt.systemPrompt
	}
	rt.systemPrompt = InjectSkillsMetadata(rt.baseSystemPrompt, setup)
}
