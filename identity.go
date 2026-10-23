package main

// Every verb finds a subscription's resources by their labels alone, since a name is only a readable handle.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

const (
	blockID = "hcloud-talos-cluster"

	keyBlock           = "mitstack.dev/block"
	keySubscription    = "mitstack.dev/subscription"
	keyRole            = "mitstack.dev/role"
	keyEnrolmentSHA256 = "mitstack.dev/enrolment-sha256"

	// maxNameLength is the longest hostname label, which a Hetzner server name must be.
	maxNameLength       = 63
	nameSuffixLength    = 8
	enrolmentTokenBytes = 32
)

// role names what a labelled resource is to the cluster, since several share a Hetzner kind.
type role string

const (
	roleGateway role = "gateway"
	roleBuilder role = "builder"
	roleNode    role = "node"
	roleImage   role = "image"
	roleAPI     role = "api"
	roleIngress role = "ingress"
)

var labelValueRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)

var lowerBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func validSubscriptionID(id string) bool {
	return labelValueRe.MatchString(id)
}

func subscriptionSelector(subscriptionID string) string {
	return keyBlock + "=" + blockID + "," + keySubscription + "=" + subscriptionID
}

// roleSelector is the label selector a load balancer finds its targets by, so a replaced node needs no edit to it.
func roleSelector(subscriptionID string, r role) string {
	return subscriptionSelector(subscriptionID) + "," + keyRole + "=" + string(r)
}

func ownerLabels(subscriptionID string, r role) map[string]string {
	return map[string]string{keyBlock: blockID, keySubscription: subscriptionID, keyRole: string(r)}
}

// resourceName is the app's name and the subscription's first characters, with the role after them.
func resourceName(appName, subscriptionID string, r role) string {
	suffix := hostnamePart(subscriptionID)
	if len(suffix) > nameSuffixLength {
		suffix = strings.Trim(suffix[:nameSuffixLength], "-")
	}
	tail := "-" + suffix + "-" + string(r)
	prefix := hostnamePart(appName)
	if room := maxNameLength - len(tail); len(prefix) > room {
		prefix = strings.Trim(prefix[:room], "-")
	}
	if prefix == "" {
		prefix = "talos"
	}
	return prefix + tail
}

// locatedName is resourceName with the location after the role, so the name says where the resource runs.
func locatedName(appName, subscriptionID string, r role, loc string) string {
	return resourceName(appName, subscriptionID, role(string(r)+"-"+loc))
}

func located(resource, loc string) string {
	return resource + "-" + loc
}

func hostnamePart(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// enrolmentTokenDigest is base32 rather than hex, since its 52 characters fit a Hetzner label value.
func enrolmentTokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return strings.ToLower(lowerBase32.EncodeToString(sum[:]))
}

func mintEnrolmentToken() (string, error) {
	raw := make([]byte, enrolmentTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("minting the enrolment token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
