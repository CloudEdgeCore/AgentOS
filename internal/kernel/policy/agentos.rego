# Agent OS v1alpha1 admission policy (Rego v1).
#
# The kernel evaluates this module with default-deny semantics: `allow` is
# false unless a rule grants it, and `deny_reasons` explains every failed
# condition so decisions stay machine-readable without model inference.
#
# Input is constructed by trusted kernel code:
#   input.task.priority            int
#   input.tenant.max_priority      int
package agentos.policy

import rego.v1

default allow := false

# An admitted task must respect the tenant's maximum priority.
allow if {
	input.task.priority <= input.tenant.max_priority
}

deny_reasons contains "TASK_PRIORITY_EXCEEDS_TENANT_MAX" if {
	input.task.priority > input.tenant.max_priority
}
