// Package github is the anti-corruption layer between GitHub and the
// Resolution domain: the webhook handler and translator inbound, the App
// client outbound. It is the only package that imports go-github and
// ghinstallation; nothing of theirs crosses into domain types.
package github
