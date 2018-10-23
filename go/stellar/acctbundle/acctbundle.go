package acctbundle

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/keybase/client/go/protocol/stellar1"
	"github.com/stellar/go/keypair"
	"golang.org/x/crypto/nacl/secretbox"
)

// New creates a BundleRestricted from an existing secret key.
func New(secret stellar1.SecretKey, name string) (*stellar1.BundleRestricted, error) {
	secretKey, accountID, _, err := libkb.ParseStellarSecretKey(string(secret))
	if err != nil {
		return nil, err
	}
	entry := newEntry(accountID, name, false)
	r := &stellar1.BundleRestricted{
		Revision:       1,
		Accounts:       []stellar1.BundleEntryRestricted{entry},
		AccountBundles: make(map[stellar1.AccountID]stellar1.AccountBundle),
	}
	r.AccountBundles[accountID] = newAccountBundle(accountID, secretKey)
	return r, nil
}

// NewInitial creates a BundleRestricted with a new random secret key.
func NewInitial(name string) (*stellar1.BundleRestricted, error) {
	full, err := keypair.Random()
	if err != nil {
		return nil, err
	}
	masterKey := stellar1.SecretKey(full.Seed())

	x, err := New(masterKey, name)
	if err != nil {
		return nil, err
	}

	x.Accounts[0].IsPrimary = true

	return x, nil
}

func NewFromBundle(bundle stellar1.Bundle) (*stellar1.BundleRestricted, error) {
	r := &stellar1.BundleRestricted{
		Revision:       bundle.Revision,
		Prev:           bundle.Prev,
		AccountBundles: make(map[stellar1.AccountID]stellar1.AccountBundle),
	}
	for _, acct := range bundle.Accounts {
		r.Accounts = append(r.Accounts, newEntry(acct.AccountID, acct.Name, acct.IsPrimary))
		// XXX multiple signers
		r.AccountBundles[acct.AccountID] = newAccountBundle(acct.AccountID, acct.Signers[0])
	}
	return r, nil
}

func newEntry(accountID stellar1.AccountID, name string, isPrimary bool) stellar1.BundleEntryRestricted {
	return stellar1.BundleEntryRestricted{
		AccountID: accountID,
		Name:      name,
		Mode:      stellar1.AccountMode_USER,
		IsPrimary: isPrimary,
	}
}

func newAccountBundle(accountID stellar1.AccountID, secretKey stellar1.SecretKey) stellar1.AccountBundle {
	return stellar1.AccountBundle{
		Revision:  1,
		AccountID: accountID,
		Signers:   []stellar1.SecretKey{secretKey},
	}
}

// BoxedEncoded is the result of boxing and encoding a BundleRestricted object.
type BoxedEncoded struct {
	EncParent           stellar1.EncryptedBundle
	EncParentB64        string // base64 msgpacked Enc
	VisParentB64        string
	FormatVersionParent stellar1.BundleVersion
	AcctBundles         map[stellar1.AccountID]AcctBoxedEncoded
}

func (b BoxedEncoded) toBundleEncodedB64() BundleEncodedB64 {
	benc := BundleEncodedB64{
		EncParent:   b.EncParentB64,
		VisParent:   b.VisParentB64,
		AcctBundles: make(map[stellar1.AccountID]string),
	}

	for acctID, acctBundle := range b.AcctBundles {
		benc.AcctBundles[acctID] = acctBundle.EncB64
	}

	return benc
}

type AcctBoxedEncoded struct {
	// Enc           stellar1.EncryptedAccountBundle
	EncB64        string // base64 msgpacked Enc
	FormatVersion stellar1.AccountBundleVersion
}

type BundleEncodedB64 struct {
	EncParent           string                        `json:"encrypted_parent"` // base64 msgpacked Enc
	VisParent           string                        `json:"visible_parent"`
	FormatVersionParent stellar1.AccountBundleVersion `json:"version_parent"`
	AcctBundles         map[stellar1.AccountID]string `json:"account_bundles"`
}

// BoxAndEncode encrypts and encodes a BundleRestricted object.
func BoxAndEncode(a *stellar1.BundleRestricted, pukGen keybase1.PerUserKeyGeneration, puk libkb.PerUserKeySeed) (*BoxedEncoded, error) {
	boxed := &BoxedEncoded{
		FormatVersionParent: stellar1.BundleVersion_V2,
	}

	accountsVisible, accountsSecret := visibilitySplit(a)

	// visible portion parent
	visibleV2 := stellar1.BundleVisibleV2{
		Revision: a.Revision,
		Prev:     a.Prev,
		Accounts: accountsVisible,
	}
	visiblePack, err := libkb.MsgpackEncode(visibleV2)
	if err != nil {
		return nil, err
	}
	visibleHash := sha256.Sum256(visiblePack)
	boxed.VisParentB64 = base64.StdEncoding.EncodeToString(visiblePack)

	// secret portion parent
	versionedSecret := stellar1.NewBundleSecretVersionedWithV2(stellar1.BundleSecretV2{
		VisibleHash: visibleHash[:],
		Accounts:    accountsSecret,
	})
	boxed.EncParent, boxed.EncParentB64, err = parentBoxAndEncode(versionedSecret, pukGen, puk)
	if err != nil {
		return nil, err
	}

	// encrypted account bundles
	boxed.AcctBundles = make(map[stellar1.AccountID]AcctBoxedEncoded)

	for _, acctEntry := range visibleV2.Accounts {
		secret, ok := a.AccountBundles[acctEntry.AccountID]
		if !ok {
			continue
		}
		ab, err := accountBoxAndEncode(acctEntry, secret, pukGen, puk)
		if err != nil {
			return nil, err
		}
		if ab != nil {
			boxed.AcctBundles[acctEntry.AccountID] = *ab
		}
	}

	return boxed, nil
}

func visibilitySplit(a *stellar1.BundleRestricted) ([]stellar1.BundleVisibleEntryV2, []stellar1.BundleSecretEntryV2) {
	vis := make([]stellar1.BundleVisibleEntryV2, len(a.Accounts))
	sec := make([]stellar1.BundleSecretEntryV2, len(a.Accounts))
	for i, acct := range a.Accounts {
		vis[i] = stellar1.BundleVisibleEntryV2{
			AccountID: acct.AccountID,
			Mode:      acct.Mode,
			IsPrimary: acct.IsPrimary,
			// XXX revision???
		}
		sec[i] = stellar1.BundleSecretEntryV2{
			Name: acct.Name,
		}
	}
	return vis, sec
}

func parentBoxAndEncode(bundle stellar1.BundleSecretVersioned, pukGen keybase1.PerUserKeyGeneration, puk libkb.PerUserKeySeed) (stellar1.EncryptedBundle, string, error) {
	// Msgpack (inner)
	clearpack, err := libkb.MsgpackEncode(bundle)
	if err != nil {
		return stellar1.EncryptedBundle{}, "", err
	}

	// Derive key
	symmetricKey, err := puk.DeriveSymmetricKey(libkb.DeriveReasonPUKStellarBundle)
	if err != nil {
		return stellar1.EncryptedBundle{}, "", err
	}

	// Secretbox
	var nonce [libkb.NaclDHNonceSize]byte
	nonce, err = libkb.RandomNaclDHNonce()
	if err != nil {
		return stellar1.EncryptedBundle{}, "", err
	}
	secbox := secretbox.Seal(nil, clearpack[:], &nonce, (*[libkb.NaclSecretBoxKeySize]byte)(&symmetricKey))

	// Annotate
	res := stellar1.EncryptedBundle{
		V:   2,
		E:   secbox,
		N:   nonce,
		Gen: pukGen,
	}

	// Msgpack (outer) + b64
	cipherpack, err := libkb.MsgpackEncode(res)
	if err != nil {
		return stellar1.EncryptedBundle{}, "", err
	}
	resB64 := base64.StdEncoding.EncodeToString(cipherpack)
	return res, resB64, nil
}

func accountBoxAndEncode(visEntry stellar1.BundleVisibleEntryV2, accountBundle stellar1.AccountBundle, pukGen keybase1.PerUserKeyGeneration, puk libkb.PerUserKeySeed) (*AcctBoxedEncoded, error) {
	visiblePack, err := libkb.MsgpackEncode(visEntry)
	if err != nil {
		return nil, err
	}
	visibleHash := sha256.Sum256(visiblePack)
	versionedSecret := stellar1.NewAccountBundleSecretVersionedWithV1(stellar1.AccountBundleSecretV1{
		VisibleHash: visibleHash[:],
		AccountID:   visEntry.AccountID,
		Signers:     accountBundle.Signers,
	})

	encBundle, b64, err := accountEncrypt(versionedSecret, pukGen, puk)
	if err != nil {
		return nil, err
	}

	/*
		clearpack, err := libkb.MsgpackEncode(versionedSecret)
		if err != nil {
			return nil, err
		}
		symmetricKey, err := puk.DeriveSymmetricKey(libkb.DeriveReasonPUKStellarBundle)
		if err != nil {
			return nil, err
		}
		var nonce [libkb.NaclDHNonceSize]byte
		nonce, err = libkb.RandomNaclDHNonce()
		if err != nil {
			return res, resB64, err
		}
		secbox := secretbox.Seal(nil, clearpack[:], &nonce, (*[libkb.NaclSecretBoxKeySize]byte)(&symmetricKey))
	*/
	_ = encBundle
	res := AcctBoxedEncoded{ /*Enc: encBundle,*/ EncB64: b64, FormatVersion: 999 /* where is this */}

	return &res, nil
}

// ErrNoChangeNecessary means that any proposed change to a bundle isn't
// actually necessary.
var ErrNoChangeNecessary = errors.New("no account mode change is necessary")

// XXX FIX
// MakeMobileOnly transforms a stellar1.AccountBundle into a mobile-only
// bundle.  This advances the revision.  If it's already mobile-only,
// this function will return ErrNoChangeNecessary.
func MakeMobileOnly(a *stellar1.BundleRestricted) error {
	/*
		if a.Mode == stellar1.AccountMode_MOBILE {
			return ErrNoChangeNecessary
		}

		a.Mode = stellar1.AccountMode_MOBILE
		a.Revision++
		a.Prev = a.OwnHash
		a.OwnHash = nil
	*/

	return nil
}

// PukFinder helps this package find puks.
type PukFinder interface {
	SeedByGeneration(m libkb.MetaContext, generation keybase1.PerUserKeyGeneration) (libkb.PerUserKeySeed, error)
}

// DecodeAndUnbox decodes the encrypted and visible encoded bundles and unboxes
// the encrypted bundle using PukFinder to find the correct puk.  It combines
// the results into a stellar1.AccountBundle.
func DecodeAndUnbox(m libkb.MetaContext, finder PukFinder, encodedBundle BundleEncodedB64) (*stellar1.BundleRestricted, stellar1.BundleVersion, error) {
	encBundle, hash, err := decodeParent(encodedBundle.EncParent)
	if err != nil {
		return nil, 0, err
	}

	puk, err := finder.SeedByGeneration(m, encBundle.Gen)
	if err != nil {
		return nil, 0, err
	}

	parent, parentVersion, err := unboxParent(encBundle, hash, encodedBundle.VisParent, puk)
	if err != nil {
		return nil, 0, err
	}
	fmt.Printf("encodedBundle.AcctBundles: %+v\n", encodedBundle.AcctBundles)
	parent.AccountBundles = make(map[stellar1.AccountID]stellar1.AccountBundle)
	for _, parentEntry := range parent.Accounts {
		if acctEncB64, ok := encodedBundle.AcctBundles[parentEntry.AccountID]; ok {
			acctBundle, err := decodeAndUnboxAcctBundle(m, finder, acctEncB64, parentEntry)
			if err != nil {
				// XXX keep going?
				return nil, 0, err
			}
			if acctBundle != nil {
				parent.AccountBundles[parentEntry.AccountID] = *acctBundle
			}
		}
	}

	return parent, parentVersion, nil
}

func decodeAndUnboxAcctBundle(m libkb.MetaContext, finder PukFinder, encB64 string, parentEntry stellar1.BundleEntryRestricted) (*stellar1.AccountBundle, error) {
	eab, hash, err := decode(encB64)
	if err != nil {
		return nil, err
	}

	puk, err := finder.SeedByGeneration(m, eab.Gen)
	if err != nil {
		return nil, err
	}
	ab, version, err := unbox(eab, hash, puk)
	if err != nil {
		return nil, err
	}

	// XXX version???
	_ = version
	return ab, nil
}

// accountEncrypt encrypts the stellar account key bundle for the PUK.
// Returns the encrypted struct and a base64 encoding for posting to the server.
// Does not check invariants.
func accountEncrypt(bundle stellar1.AccountBundleSecretVersioned, pukGen keybase1.PerUserKeyGeneration, puk libkb.PerUserKeySeed) (res stellar1.EncryptedAccountBundle, resB64 string, err error) {
	// Msgpack (inner)
	clearpack, err := libkb.MsgpackEncode(bundle)
	if err != nil {
		return res, resB64, err
	}

	// Derive key
	symmetricKey, err := puk.DeriveSymmetricKey(libkb.DeriveReasonPUKStellarBundle)
	if err != nil {
		return res, resB64, err
	}

	// Secretbox
	var nonce [libkb.NaclDHNonceSize]byte
	nonce, err = libkb.RandomNaclDHNonce()
	if err != nil {
		return res, resB64, err
	}
	secbox := secretbox.Seal(nil, clearpack[:], &nonce, (*[libkb.NaclSecretBoxKeySize]byte)(&symmetricKey))

	// Annotate
	res = stellar1.EncryptedAccountBundle{
		V:   1,
		E:   secbox,
		N:   nonce,
		Gen: pukGen,
	}

	// Msgpack (outer) + b64
	cipherpack, err := libkb.MsgpackEncode(res)
	if err != nil {
		return res, resB64, err
	}
	resB64 = base64.StdEncoding.EncodeToString(cipherpack)
	return res, resB64, nil
}

// decodeParent decodes a base64-encoded encrypted parent bundle.
func decodeParent(encryptedB64 string) (stellar1.EncryptedBundle, stellar1.Hash, error) {
	cipherpack, err := base64.StdEncoding.DecodeString(encryptedB64)
	if err != nil {
		return stellar1.EncryptedBundle{}, stellar1.Hash{}, err
	}
	encHash := sha256.Sum256(cipherpack)
	var enc stellar1.EncryptedBundle
	if err = libkb.MsgpackDecode(&enc, cipherpack); err != nil {
		return stellar1.EncryptedBundle{}, stellar1.Hash{}, err
	}
	return enc, encHash[:], nil
}

// unboxParent unboxes an encrypted parent bundle and decodes the visual portion of the bundle.
// It validates the visible hash in the secret portion.
func unboxParent(encBundle stellar1.EncryptedBundle, hash stellar1.Hash, visB64 string, puk libkb.PerUserKeySeed) (*stellar1.BundleRestricted, stellar1.BundleVersion, error) {
	versioned, err := decryptParent(encBundle, puk)
	if err != nil {
		return nil, 0, err
	}
	version, err := versioned.Version()
	if err != nil {
		return nil, 0, err
	}

	var bundleOut stellar1.BundleRestricted
	switch version {
	case stellar1.BundleVersion_V2:
		visiblePack, err := base64.StdEncoding.DecodeString(visB64)
		if err != nil {
			return nil, 0, err
		}
		visibleHash := sha256.Sum256(visiblePack)
		secretV2 := versioned.V2()
		if !hmac.Equal(visibleHash[:], secretV2.VisibleHash) {
			return nil, 0, errors.New("corrupted bundle: visible hash mismatch")
		}
		var visibleV2 stellar1.BundleVisibleV2
		err = libkb.MsgpackDecode(&visibleV2, visiblePack)
		if err != nil {
			return nil, 0, err
		}
		bundleOut, err = merge(secretV2, visibleV2)
		if err != nil {
			return nil, 0, err
		}
	default:
		return nil, 0, fmt.Errorf("unsupported parent bundler version: %d", version)
	}

	bundleOut.OwnHash = hash
	if len(bundleOut.OwnHash) == 0 {
		return nil, 0, errors.New("stellar account bundle missing own hash")
	}

	return &bundleOut, version, nil
}

// decryptParent decrypts an encrypted parent bundle with the provided puk.
func decryptParent(encBundle stellar1.EncryptedBundle, puk libkb.PerUserKeySeed) (stellar1.BundleSecretVersioned, error) {
	var empty stellar1.BundleSecretVersioned
	if encBundle.V != 2 {
		return empty, fmt.Errorf("invalid stellar secret account bundle encryption version: %v", encBundle.V)
	}

	// Derive key
	reason := libkb.DeriveReasonPUKStellarBundle
	symmetricKey, err := puk.DeriveSymmetricKey(reason)
	if err != nil {
		return empty, err
	}

	// Secretbox
	clearpack, ok := secretbox.Open(nil, encBundle.E,
		(*[libkb.NaclDHNonceSize]byte)(&encBundle.N),
		(*[libkb.NaclSecretBoxKeySize]byte)(&symmetricKey))
	if !ok {
		return empty, errors.New("stellar bundle secret box open failed")
	}

	// Msgpack (inner)
	var bver stellar1.BundleSecretVersioned
	err = libkb.MsgpackDecode(&bver, clearpack)
	if err != nil {
		return empty, err
	}
	return bver, nil
}

// decode decodes a base64-encoded encrypted account bundle.
func decode(encryptedB64 string) (stellar1.EncryptedAccountBundle, stellar1.Hash, error) {
	cipherpack, err := base64.StdEncoding.DecodeString(encryptedB64)
	if err != nil {
		return stellar1.EncryptedAccountBundle{}, stellar1.Hash{}, err
	}
	encHash := sha256.Sum256(cipherpack)
	var enc stellar1.EncryptedAccountBundle
	if err = libkb.MsgpackDecode(&enc, cipherpack); err != nil {
		return stellar1.EncryptedAccountBundle{}, stellar1.Hash{}, err
	}
	return enc, encHash[:], nil
}

// unbox unboxes an encrypted account bundle and decodes the visual portion of the bundle.
// It validates the visible hash in the secret portion.
func unbox(encBundle stellar1.EncryptedAccountBundle, hash stellar1.Hash /* visB64 string, */, puk libkb.PerUserKeySeed) (*stellar1.AccountBundle, stellar1.AccountBundleVersion, error) {
	versioned, err := decrypt(encBundle, puk)
	if err != nil {
		return nil, 0, err
	}
	version, err := versioned.Version()
	if err != nil {
		return nil, 0, err
	}

	var bundleOut stellar1.AccountBundle
	switch version {
	case stellar1.AccountBundleVersion_V1:
		/*
			visiblePack, err := base64.StdEncoding.DecodeString(visB64)
			if err != nil {
				return nil, 0, err
			}
			visibleHash := sha256.Sum256(visiblePack)
		*/
		secretV1 := versioned.V1()
		/*
			if !hmac.Equal(visibleHash[:], secretV1.VisibleHash) {
				return nil, 0, errors.New("corrupted bundle: visible hash mismatch")
			}
			var visibleV1 stellar1.AccountBundleVisibleV1
			err = libkb.MsgpackDecode(&visibleV1, visiblePack)
			if err != nil {
				return nil, 0, err
			}
		*/
		// bundleOut, err = merge(secretV1, visibleV1)
		// if err != nil {
		//	return nil, 0, err
		// }
		bundleOut = stellar1.AccountBundle{Signers: secretV1.Signers}
	default:
		return nil, 0, err
	}

	bundleOut.OwnHash = hash
	if len(bundleOut.OwnHash) == 0 {
		return nil, 0, errors.New("stellar account bundle missing own hash")
	}

	return &bundleOut, version, nil
}

// decrypt decrypts an encrypted account bundle with the provided puk.
func decrypt(encBundle stellar1.EncryptedAccountBundle, puk libkb.PerUserKeySeed) (stellar1.AccountBundleSecretVersioned, error) {
	var empty stellar1.AccountBundleSecretVersioned
	if encBundle.V != 1 {
		return empty, errors.New("invalid stellar secret account bundle encryption version")
	}

	// Derive key
	reason := libkb.DeriveReasonPUKStellarBundle
	symmetricKey, err := puk.DeriveSymmetricKey(reason)
	if err != nil {
		return empty, err
	}

	// Secretbox
	clearpack, ok := secretbox.Open(nil, encBundle.E,
		(*[libkb.NaclDHNonceSize]byte)(&encBundle.N),
		(*[libkb.NaclSecretBoxKeySize]byte)(&symmetricKey))
	if !ok {
		return empty, errors.New("stellar bundle secret box open failed")
	}

	// Msgpack (inner)
	var bver stellar1.AccountBundleSecretVersioned
	err = libkb.MsgpackDecode(&bver, clearpack)
	if err != nil {
		return empty, err
	}
	return bver, nil
}

func convertVisibleAccounts(in []stellar1.BundleVisibleEntryV2) []stellar1.BundleEntryRestricted {
	out := make([]stellar1.BundleEntryRestricted, len(in))
	for i, e := range in {
		out[i] = stellar1.BundleEntryRestricted{
			AccountID: e.AccountID,
			Mode:      e.Mode,
			IsPrimary: e.IsPrimary,
		}
	}
	return out
}

// merge combines the versioned secret account bundle and the visible account bundle into
// a stellar1.AccountBundle for local use.
func merge(secret stellar1.BundleSecretV2, visible stellar1.BundleVisibleV2) (stellar1.BundleRestricted, error) {
	fmt.Printf("secret: %+v\n", secret)
	fmt.Printf("visible: %+v\n", visible)
	return stellar1.BundleRestricted{
		Revision: visible.Revision,
		Prev:     visible.Prev,
		Accounts: convertVisibleAccounts(visible.Accounts),
		/*
			AccountID: visible.AccountID,
			Mode:      visible.Mode,
			Signers:   secret.Signers,
			Name:      secret.Name,
		*/
	}, nil
}
