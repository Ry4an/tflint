package plugin

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	//nolint:staticcheck
	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/swag"
	"github.com/mitchellh/go-homedir"
	rekor "github.com/sigstore/rekor/pkg/client"
	"github.com/sigstore/rekor/pkg/generated/client/entries"
	"github.com/sigstore/rekor/pkg/generated/models"
	hashedrekord_v001 "github.com/sigstore/rekor/pkg/types/hashedrekord/v0.0.1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"golang.org/x/crypto/openpgp"
)

// SignatureChecker checks the signature of GitHub releases.
// Determines whether to select a signing key or skip it based on the InstallConfig.
type SignatureChecker struct {
	config *InstallConfig
}

// NewSignatureChecker returns a new SignatureChecker from passed InstallConfig.
func NewSignatureChecker(config *InstallConfig) *SignatureChecker {
	return &SignatureChecker{config: config}
}

// GetSigningKey returns an ASCII armored signing key.
// If the plugin is under the terraform-linters organization, you can use the built-in key even if the signing_key is omitted.
func (c *SignatureChecker) GetSigningKey() string {
	if c.config.SigningKey != "" {
		return c.config.SigningKey
	}
	if c.config.SourceOwner == "terraform-linters" {
		return builtinSigningKey
	}
	return c.config.SigningKey
}

// HasSigningKey determines whether the checker should verify the signature.
// Skip verification if no signing key is set.
func (c *SignatureChecker) HasSigningKey() bool {
	return c.GetSigningKey() != ""
}

// Verify returns the results of signature verification.
// The signing key must be ASCII armored and the signature must be in binary OpenPGP format.
func (c *SignatureChecker) Verify(target, signature io.Reader) error {
	key := c.GetSigningKey()
	if key == "" {
		return fmt.Errorf("No signing key configured")
	}

	reader := strings.NewReader(key)
	keyring, err := openpgp.ReadArmoredKeyRing(reader)
	if err != nil {
		return err
	}

	_, err = openpgp.CheckDetachedSignature(keyring, target, signature)
	if err != nil {
		return err
	}
	return nil
}

var rekorURL string = "https://rekor.sigstore.dev"
var tufRootURL string = "tuf-repo-cdn.sigstore.dev"
var githubActionsOIDIssuer string = "https://token.actions.githubusercontent.com"
var tufPath string = "~/.tflint.d/tufdata"

// In sigstore-go, SignedEntity is assumed to be a parsed Sigstore bundle,
// but if you implement the interface, it can also be used for entities signed by Cosign.
// @see https://github.com/sigstore/sigstore-go/blob/v0.1.0/docs/verification.md#abstractions
type signedEntity struct {
	verify.BaseSignedEntity

	artifact    []byte
	certificate []byte
	signature   []byte
	tlogs       []*tlog.Entry
}

var _ verify.SignedEntity = (*signedEntity)(nil)

func newSignedEntity(artifact, certificate, signature io.ReadSeeker) (*signedEntity, error) {
	art, err := io.ReadAll(artifact)
	if err != nil {
		return nil, err
	}
	if _, err := artifact.Seek(0, 0); err != nil {
		return nil, err
	}

	encodedCert, err := io.ReadAll(certificate)
	if err != nil {
		return nil, err
	}
	if _, err := certificate.Seek(0, 0); err != nil {
		return nil, err
	}
	cert, err := base64.StdEncoding.DecodeString(string(encodedCert))
	if err != nil {
		return nil, err
	}

	encodedSig, err := io.ReadAll(signature)
	if err != nil {
		return nil, err
	}
	if _, err := signature.Seek(0, 0); err != nil {
		return nil, err
	}
	sig, err := base64.StdEncoding.DecodeString(string(encodedSig))
	if err != nil {
		return nil, err
	}

	logs, err := findTLogEntries(art, cert, sig)
	if err != nil {
		return nil, err
	}

	return &signedEntity{
		artifact:    art,
		certificate: cert,
		signature:   sig,
		tlogs:       logs,
	}, nil
}

// findTLogEntries searches transparency logs for the artifact signed by Cosign.
// This is inspired by Cosign's implementation and is equivalent to the logic to get transparency logs
// in sigstore-go's WithOnlineVerification.
// https://github.com/sigstore/cosign/blob/v2.2.0/pkg/cosign/tlog.go#L407
// https://github.com/sigstore/sigstore-go/blob/v0.1.0/pkg/verify/tlog.go#L115-L162
//
// TODO: Is this safe when the artifact is signed multiple times? Do we need filtering like below?
// @see https://github.com/sigstore/sigstore-go/blob/v0.1.0/pkg/verify/tlog.go#L164-L179
func findTLogEntries(artifact, certificate, signature []byte) ([]*tlog.Entry, error) {
	searchParams := entries.NewSearchLogQueryParamsWithContext(context.Background())
	searchLogQuery := models.SearchLogQuery{}

	sha256CheckSum := sha256.New()
	if _, err := sha256CheckSum.Write(artifact); err != nil {
		return nil, err
	}
	re := hashedrekord_v001.V001Entry{
		HashedRekordObj: models.HashedrekordV001Schema{
			Data: &models.HashedrekordV001SchemaData{
				Hash: &models.HashedrekordV001SchemaDataHash{
					Algorithm: swag.String(models.HashedrekordV001SchemaDataHashAlgorithmSha256),
					Value:     swag.String(hex.EncodeToString(sha256CheckSum.Sum(nil))),
				},
			},
			Signature: &models.HashedrekordV001SchemaSignature{
				Content: strfmt.Base64(signature),
				PublicKey: &models.HashedrekordV001SchemaSignaturePublicKey{
					Content: strfmt.Base64(certificate),
				},
			},
		},
	}
	entry := &models.Hashedrekord{
		APIVersion: swag.String(re.APIVersion()),
		Spec:       re.HashedRekordObj,
	}
	proposedEntries := []models.ProposedEntry{entry}

	searchLogQuery.SetEntries(proposedEntries)
	searchParams.SetEntry(&searchLogQuery)

	rekorClient, err := rekor.GetRekorClient(rekorURL)
	if err != nil {
		return nil, err
	}
	resp, err := rekorClient.Entries.SearchLogQuery(searchParams)
	if err != nil {
		return nil, fmt.Errorf("searching log query: %w", err)
	}
	if len(resp.Payload) == 0 {
		return nil, errors.New("signature not found in transparency log")
	}

	logs := []*tlog.Entry{}
	for _, logEntry := range resp.GetPayload() {
		for _, e := range logEntry {
			decodedBody, err := base64.StdEncoding.DecodeString(e.Body.(string))
			if err != nil {
				return nil, err
			}
			decodedLogId, err := hex.DecodeString(*e.LogID)
			if err != nil {
				return nil, err
			}

			log, err := tlog.NewEntry(
				decodedBody,
				*e.IntegratedTime,
				*e.LogIndex,
				decodedLogId,
				e.Verification.SignedEntryTimestamp,
				e.Verification.InclusionProof,
			)
			if err != nil {
				return nil, err
			}
			logs = append(logs, log)
		}
	}

	return logs, nil
}

// HasInclustionPromise seems to be a flag that determines whether to verify SET,
// but the entity flag is not used in sigstore-go, and only the Tlog flag seems to be checked.
// @see https://github.com/sigstore/sigstore-go/blob/v0.1.0/pkg/verify/tlog.go#L84
//
// So it probably doesn't make sense to return this flag either way.
func (e *signedEntity) HasInclusionPromise() bool {
	return true
}

// When HasInclustionProof returns true, Rekor's public key is used to verify STH
// and to verify that the Tlog has not been tampered with.
// @see https://github.com/sigstore/sigstore-go/blob/v0.1.0/pkg/verify/tlog.go#L94-L113
func (e *signedEntity) HasInclusionProof() bool {
	return true
}

func (e *signedEntity) SignatureContent() (verify.SignatureContent, error) {
	return bundle.NewMessageSignature(nil, "", e.signature), nil
}

func (e *signedEntity) VerificationContent() (verify.VerificationContent, error) {
	block, _ := pem.Decode(e.certificate)
	if block == nil {
		return nil, fmt.Errorf("failed to parse PEM block")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}

	return &bundle.CertificateChain{Certificates: []*x509.Certificate{certificate}}, nil
}

func (e *signedEntity) TlogEntries() ([]*tlog.Entry, error) {
	return e.tlogs, nil
}

func (c *SignatureChecker) VerifyKeyless(artifact, certificate, signature io.ReadSeeker) error {
	entity, err := newSignedEntity(artifact, certificate, signature)
	if err != nil {
		return err
	}

	verifierConfig := []verify.VerifierOption{}
	// Verify SCT using the CT log server's public key to ensure that the certificate was issued by Fulcio in a legitimate manner.
	// @see https://github.com/sigstore/sigstore-go/blob/v0.1.0/pkg/verify/signed_entity.go#L516-L521
	verifierConfig = append(verifierConfig, verify.WithSignedCertificateTimestamps(1))
	// Verify SET using the Rekor's public key to ensure that the short-lived certificate was valid when the artifact was signed.
	// @see https://github.com/sigstore/sigstore-go/blob/v0.1.0/pkg/verify/signed_entity.go#L616-L627
	// @see https://github.com/sigstore/sigstore-go/blob/v0.1.0/pkg/verify/signed_entity.go#L654-L659
	verifierConfig = append(verifierConfig, verify.WithTransparencyLog(1))
	verifierConfig = append(verifierConfig, verify.WithIntegratedTimestamps(1))
	// Note that WithOnlineVerification is not enabled. If online validation is enabled, Tlog will be retrieved based on the log ID
	// to verify SET and inclusion proof. But this is the same as what we're doing with findTLogEntries.
	// If not enabled, SET and inclusion proof are verified against TlogEntries. That's enough.
	// @see https://github.com/sigstore/sigstore-go/blob/v0.1.0/pkg/verify/tlog.go#L81-L113

	// Verify certificate identity to ensure that the signature was made in GitHub Actions for the repository.
	// @see https://github.com/sigstore/sigstore-go/blob/v0.1.0/pkg/verify/signed_entity.go#L587-L604
	identityPolicies := []verify.PolicyOption{}
	expectedSANRegex := fmt.Sprintf("^https://%s/%s/%s/", c.config.SourceHost, c.config.SourceOwner, c.config.SourceRepo)
	certID, err := verify.NewShortCertificateIdentity(githubActionsOIDIssuer, "", "", expectedSANRegex)
	if err != nil {
		return err
	}
	identityPolicies = append(identityPolicies, verify.WithCertificateIdentity(certID))

	// Get the public keys of Rekor, Fulcio, CTlog server.
	// These are the roots and the ends of the chain of trust.
	workPath, err := homedir.Expand(tufPath)
	if err != nil {
		return err
	}
	var trustedMaterial = make(root.TrustedMaterialCollection, 0)
	var trustedrootJSON []byte
	trustedrootJSON, err = tuf.GetTrustedrootJSON(tufRootURL, workPath)
	if err != nil {
		return err
	}
	var trustedRoot *root.TrustedRoot
	trustedRoot, err = root.NewTrustedRootFromJSON(trustedrootJSON)
	if err != nil {
		return err
	}
	trustedMaterial = append(trustedMaterial, trustedRoot)

	// Verify signature to ensure that the artifact was signed by the certificate.
	// @see https://github.com/sigstore/sigstore-go/blob/v0.1.0/pkg/verify/signed_entity.go#L538-L548
	verifier, err := verify.NewSignedEntityVerifier(trustedMaterial, verifierConfig...)
	if err != nil {
		return err
	}
	artifactPolicy := verify.WithArtifact(artifact)
	if _, err := artifact.Seek(0, 0); err != nil {
		return err
	}

	// In short, here's what Verify does:
	//
	// 1. Verify SET in VerifyObserverTimestamps. The SET is embedded in the Tlog, which can be retrieved
	//    from Rekor based on the artifact's SHA-256. The SET and T logs cannot be tampered with because
	//    they are verified using Rekor's root public key.
	// 2. Verify certificate chain in VerifyLeafCertificate. This allows you to verify that the certificate
	//    passed was issued by Fulcio using Fulcio's root public key. At the same time, it verifies that
	//    the certificate was valid in SET and ensures that the leaked and revoked short-lived certificate
	//    has not been reused.
	// 3. Verify SCT in VerifySignedCertificateTimestamp. This ensures that the certificate was recorded
	//    in CT log servers. The SCT is verified using the CT log server's root public key, so it cannot be tampered with.
	// 4. Verify signature in VerifySignatureWithArtifact. Since the certificate's public key is guaranteed
	//    in previous steps, we use it to verify that the artifact is signed.
	// 5. Verify certificate identity in certificateIdentities.Verify. Since anyone can issue a certificate,
	//    the final step is to verify the certificate's identity. The identity is embedded in a certificate
	//    signed by Fulcio, so there is no room for tampering.
	resp, err := verifier.Verify(entity, verify.NewPolicy(artifactPolicy, identityPolicies...))
	if err != nil {
		return err
	}

	marshaled, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	log.Printf("[DEBUG] verification result=%s", string(marshaled))

	return nil
}

// builtinSigningKey is the default signing key that applies only to plugins under the terraform-linters organization.
// This makes it possible for the plugins we distribute to be used safely without having to set signing key.
var builtinSigningKey string = `
-----BEGIN PGP PUBLIC KEY BLOCK-----

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
=ZZbg
-----END PGP PUBLIC KEY BLOCK-----`
