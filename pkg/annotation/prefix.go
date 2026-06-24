// Package annotation provides the shared annotation-prefix logic used by both
// the containerd snapshotter resolver and the mutating admission webhook: a
// single configurable DNS-subdomain prefix from which every pv-snapshotter
// annotation key is derived. Annotation key suffixes remain fixed and are
// defined by each consumer.
package annotation

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// flagPrefix is the flag / viper config key that holds the annotation prefix.
	flagPrefix = "annotation-prefix"

	// defaultPrefix is the default DNS subdomain prefix for all pv-snapshotter
	// pod annotations.
	defaultPrefix = "pv-snapshotter.humble-mun.io"

	// reservedPrefixKubernetes and reservedPrefixK8s are the two
	// Kubernetes-reserved DNS subdomains that must not be used as the
	// pv-snapshotter annotation prefix.
	reservedPrefixKubernetes = "kubernetes.io"
	reservedPrefixK8s        = "k8s.io"
)

// RegisterFlags registers the shared --annotation-prefix flag. Both the
// snapshotter resolver and the mutating webhook derive their annotation keys
// from this single prefix, so it is registered once here rather than by either
// consumer.
func RegisterFlags(pfs *pflag.FlagSet) {
	pfs.String(flagPrefix, defaultPrefix,
		"DNS subdomain prefix for all pv-snapshotter pod annotations (must be a valid "+
			"RFC 1123 DNS subdomain, no trailing slash). Annotation keys are derived from "+
			"this prefix at startup, for example:\n"+
			"  <prefix>/upperdir-path          – literal upperdir root path\n"+
			"  <prefix>/upperdir-path-template – Go template rendered to upperdir root path\n"+
			"  <prefix>/var.<Name>             – template variable injected into template data\n"+
			"  <prefix>/pvc-name-template      – per-pod PVC name override for the webhook")
}

// validatePrefix checks that prefix is a valid Kubernetes annotation key prefix
// (RFC 1123 DNS subdomain, ≤253 chars, no trailing slash).
//
// The reserved prefixes "kubernetes.io" and "k8s.io" are rejected to avoid
// conflicts with built-in annotations.
func validatePrefix(prefix string) (err error) {
	if strings.HasSuffix(prefix, "/") {
		err = fmt.Errorf("annotation prefix must not include a trailing slash: %q", prefix)
		return
	}
	if errs := validation.IsDNS1123Subdomain(prefix); len(errs) > 0 {
		err = fmt.Errorf("annotation prefix %q is not a valid DNS subdomain: %s",
			prefix, strings.Join(errs, "; "))
		return
	}
	if prefix == reservedPrefixKubernetes || prefix == reservedPrefixK8s ||
		strings.HasSuffix(prefix, "."+reservedPrefixKubernetes) || strings.HasSuffix(prefix, "."+reservedPrefixK8s) {
		err = fmt.Errorf("annotation prefix %q uses a reserved Kubernetes domain", prefix)
		return
	}
	return
}

// ResolvePrefix reads the configured annotation prefix from viper and validates it.
func ResolvePrefix() (prefix string, err error) {
	prefix = viper.GetString(flagPrefix)
	if err = validatePrefix(prefix); err != nil {
		err = fmt.Errorf("invalid --%s: %w", flagPrefix, err)
		return
	}
	return
}

// Key joins a validated annotation prefix and a suffix into a full annotation
// key of the form "<prefix>/<suffix>".
func Key(prefix, suffix string) string {
	return prefix + "/" + suffix
}
