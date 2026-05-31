package command

import "github.com/tum-zulip/go-zulip/zulip"

// SubcmdSpec maps subcommand words to either a leaf arg struct (any non-nil
// struct value), a nested SubcmdSpec (for further dispatch), or nil (leaf
// with zero remaining args expected, mapped to NoArgs).
// An empty-string key "" handles the "no subcommand token given" case.
type SubcmdSpec map[string]any

// NoArgs is the result type for commands and subcommands that take no arguments.
type NoArgs struct{}

// RestrictedSpec marks an argument spec subtree as requiring a minimum role.
// Routers can use this to authorize a selected subcommand before parsing its
// leaf arguments.
type RestrictedSpec struct {
	Permission zulip.Role
	Spec       any
}

func RequireRole(permission zulip.Role, spec any) RestrictedSpec {
	return RestrictedSpec{Permission: permission, Spec: spec}
}

func RequiredPermission(spec any, rawArgs []string) zulip.Role {
	var permission zulip.Role
	for {
		if restricted, ok := spec.(RestrictedSpec); ok {
			permission = stricterPermission(permission, restricted.Permission)
			spec = restricted.Spec
			continue
		}

		sub, ok := spec.(SubcmdSpec)
		if !ok {
			return permission
		}
		if len(rawArgs) == 0 {
			nested, ok := sub[""]
			if !ok {
				return permission
			}
			spec = nested
			continue
		}
		nested, ok := sub[rawArgs[0]]
		if !ok {
			return permission
		}
		rawArgs = rawArgs[1:]
		spec = nested
	}
}

func FilterArgSpec(spec any, allowed func(zulip.Role) bool) any {
	filtered, _ := filterArgSpec(spec, allowed)
	return filtered
}

func filterArgSpec(spec any, allowed func(zulip.Role) bool) (any, bool) {
	if restricted, ok := spec.(RestrictedSpec); ok {
		if !allowed(restricted.Permission) {
			return nil, false
		}
		return filterArgSpec(restricted.Spec, allowed)
	}

	sub, ok := spec.(SubcmdSpec)
	if !ok {
		return spec, true
	}

	filtered := make(SubcmdSpec, len(sub))
	for key, nested := range sub {
		visible, ok := filterArgSpec(nested, allowed)
		if ok {
			filtered[key] = visible
		}
	}
	return filtered, true
}

func stricterPermission(a, b zulip.Role) zulip.Role {
	if a == PermOpen {
		return b
	}
	if b == PermOpen {
		return a
	}
	if a < b {
		return a
	}
	return b
}
