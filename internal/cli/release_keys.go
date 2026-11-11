package cli

// releaseKeys are the public keys whose signatures `awxctl upgrade` accepts on a
// release's checksums.txt. Each line is an ssh-ed25519 public key. More than
// one key may be listed so a key can be rotated: ship a release that trusts both,
// then sign with the new one.
//
// The matching private key is the AWXCTL_SIGNING_KEY secret of the release
// workflow; it signs checksums.txt (ssh-keygen -Y sign -n awxctl-release).
var releaseKeys = []string{
	// release signing key, fingerprint SHA256:JcxegRoINrLo8XEppZs2uirJPaVY20R2BN0jtugTZTY
	"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIFF0yT3P/A6R2kLYJ3Bf1O+iZVLPFFD53wyAdG7Mkhd awxctl signing",
}
