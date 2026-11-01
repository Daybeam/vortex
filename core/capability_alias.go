package core

import (
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/capability"
)

// Transitional bridge for callers not yet migrated to core/capability.
type (
	CapabilityNavigator = capability.CapabilityNavigator
	CapabilityIndex     = capability.CapabilityIndex
	CapabilityDriver    = capability.CapabilityDriver
	DiscoveryResult     = capability.DiscoveryResult
	DiscoveryScope      = capability.DiscoveryScope
	FindSkillsResult    = capability.FindSkillsResult
	CandidateType       = capability.CandidateType
	DiscoveryCandidate  = capability.DiscoveryCandidate
)

const (
	CandidateRole       = capability.CandidateRole
	CandidateSkill      = capability.CandidateSkill
	CandidateGroup      = capability.CandidateGroup
	CandidateSOP        = capability.CandidateSOP
	CandidateMCP        = capability.CandidateMCP
	CandidateCapability = capability.CandidateCapability
)

func NewCapabilityNavigator(reg *config.Registry) *CapabilityNavigator {
	return capability.NewCapabilityNavigator(reg)
}

func NewCapabilityIndex(reg *config.Registry) *CapabilityIndex {
	return capability.NewCapabilityIndex(reg)
}

func NewCapabilityDriver(def config.CapabilityDef, reg *config.Registry) (CapabilityDriver, error) {
	return capability.NewCapabilityDriver(def, reg)
}

func ParseScope(scopeStr, callerRoleID string, allowPool bool) (DiscoveryScope, error) {
	return capability.ParseScope(scopeStr, callerRoleID, allowPool)
}

func DiscoverImpl(reg *config.Registry, scope DiscoveryScope, dtype, query string, page, pageSize int) (*DiscoveryResult, error) {
	return capability.DiscoverImpl(reg, scope, dtype, query, page, pageSize)
}

func FindSkillsImpl(reg *config.Registry, query, capName, provides string, page, pageSize int) (*FindSkillsResult, error) {
	return capability.FindSkillsImpl(reg, query, capName, provides, page, pageSize)
}
