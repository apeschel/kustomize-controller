// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package azkv

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"time"
	"unicode/utf16"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/keyvault/azkeys/crypto"
	"github.com/dimchansky/utfbom"
	"sigs.k8s.io/yaml"
)

// MasterKey is an Azure Key Vault key used to encrypt and decrypt SOPS' data key.
// The underlying authentication token can be configured using AADConfig.
type MasterKey struct {
	VaultURL string
	Name     string
	Version  string

	EncryptedKey string
	CreationDate time.Time

	token azcore.TokenCredential
}

// LoadAADConfigFromBytes attempts to load the given bytes into the given AADConfig.
// By first decoding it if UTF-16, and then unmarshalling it into the given struct.
// It returns an error for any failure.
func LoadAADConfigFromBytes(b []byte, s *AADConfig) error {
	b, err := decode(b)
	if err != nil {
		return fmt.Errorf("failed to decode Azure authentication file bytes: %w", err)
	}
	if err = yaml.Unmarshal(b, s); err != nil {
		err = fmt.Errorf("failed to unmarshal Azure authentication file: %w", err)
	}
	return err
}

// AADConfig contains the selection of fields from an Azure authentication file
// required for Active Directory authentication.
type AADConfig struct {
	AZConfig
	TenantID                   string `json:"tenantId,omitempty"`
	ClientID                   string `json:"clientId,omitempty"`
	ClientSecret               string `json:"clientSecret,omitempty"`
	ClientCertificate          string `json:"clientCertificate,omitempty"`
	ClientCertificatePassword  string `json:"clientCertificatePassword,omitempty"`
	ClientCertificateSendChain bool   `json:"clientCertificateSendChain,omitempty"`
	AuthorityHost              string `json:"authorityHost,omitempty"`
}

// AZConfig contains the Service Principal fields as generated by `az`.
// Ref: https://docs.microsoft.com/en-us/azure/aks/kubernetes-service-principal?tabs=azure-cli#manually-create-a-service-principal
type AZConfig struct {
	AppID    string `json:"appId,omitempty"`
	Tenant   string `json:"tenant,omitempty"`
	Password string `json:"password,omitempty"`
}

// SetToken attempts to configure the token on the MasterKey using the
// AADConfig values. It detects credentials in the following order:
//
//  - azidentity.ClientSecretCredential when `tenantId`, `clientId` and
//    `clientSecret` fields are found.
//  - azidentity.ClientCertificateCredential when `tenantId`,
//    `clientCertificate` (and optionally `clientCertificatePassword`) fields
//    are found.
//  - azidentity.ClientSecretCredential when AZConfig fields are found.
//  - azidentity.ManagedIdentityCredential for a User ID, when a `clientId`
//    field but no `tenantId` is found.
//
// If no set of credentials is found or the azcore.TokenCredential can not be
// created, an error is returned.
func (s *AADConfig) SetToken(key *MasterKey) error {
	if s == nil || key == nil {
		return nil
	}

	var err error
	if s.TenantID != "" && s.ClientID != "" {
		if s.ClientSecret != "" {
			key.token, err = azidentity.NewClientSecretCredential(s.TenantID, s.ClientID, s.ClientSecret, &azidentity.ClientSecretCredentialOptions{
				AuthorityHost: s.GetAuthorityHost(),
			})
			return err
		}
		if s.ClientCertificate != "" {
			certs, pk, err := azidentity.ParseCertificates([]byte(s.ClientCertificate), []byte(s.ClientCertificatePassword))
			key.token, err = azidentity.NewClientCertificateCredential(s.TenantID, s.ClientID, certs, pk, &azidentity.ClientCertificateCredentialOptions{
				SendCertificateChain: s.ClientCertificateSendChain,
				AuthorityHost:        s.GetAuthorityHost(),
			})
			return err
		}
	}
	if s.Tenant != "" && s.AppID != "" && s.Password != "" {
		key.token, err = azidentity.NewClientSecretCredential(s.Tenant, s.AppID, s.Password, &azidentity.ClientSecretCredentialOptions{
			AuthorityHost: s.GetAuthorityHost(),
		})
		return err
	}
	if s.ClientID != "" {
		key.token, err = azidentity.NewManagedIdentityCredential(&azidentity.ManagedIdentityCredentialOptions{
			ID: azidentity.ClientID(s.ClientID),
		})
		return err
	}

	return fmt.Errorf("invalid data: requires a '%s' field, a combination of '%s', '%s' and '%s', or '%s', '%s' and '%s'",
		"clientId", "tenantId", "clientId", "clientSecret", "tenantId", "clientId", "clientCertificate")
}

// GetAuthorityHost returns the AuthorityHost, or the Azure Public Cloud
// default.
func (s *AADConfig) GetAuthorityHost() azidentity.AuthorityHost {
	if s.AuthorityHost != "" {
		return azidentity.AuthorityHost(s.AuthorityHost)
	}
	return azidentity.AzurePublicCloud
}

// EncryptedDataKey returns the encrypted data key this master key holds.
func (key *MasterKey) EncryptedDataKey() []byte {
	return []byte(key.EncryptedKey)
}

// SetEncryptedDataKey sets the encrypted data key for this master key.
func (key *MasterKey) SetEncryptedDataKey(enc []byte) {
	key.EncryptedKey = string(enc)
}

// Encrypt takes a SOPS data key, encrypts it with Key Vault and stores the result in the EncryptedKey field.
func (key *MasterKey) Encrypt(dataKey []byte) error {
	c, err := crypto.NewClient(key.ToString(), key.token, nil)
	if err != nil {
		return fmt.Errorf("failed to construct client to encrypt data: %w", err)
	}
	resp, err := c.Encrypt(context.Background(), crypto.AlgorithmRSAOAEP256, dataKey, nil)
	if err != nil {
		return fmt.Errorf("failed to encrypt data: %w", err)
	}
	key.EncryptedKey = string(resp.Result)
	return nil
}

// EncryptIfNeeded encrypts the provided SOPS' data key and encrypts it if it hasn't been encrypted yet.
func (key *MasterKey) EncryptIfNeeded(dataKey []byte) error {
	if key.EncryptedKey == "" {
		return key.Encrypt(dataKey)
	}
	return nil
}

// Decrypt decrypts the EncryptedKey field with Azure Key Vault and returns the result.
func (key *MasterKey) Decrypt() ([]byte, error) {
	c, err := crypto.NewClient(key.ToString(), key.token, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to construct client to decrypt data: %w", err)
	}
	resp, err := c.Decrypt(context.Background(), crypto.AlgorithmRSAOAEP256, []byte(key.EncryptedKey), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt data: %w", err)
	}
	return resp.Result, nil
}

// NeedsRotation returns whether the data key needs to be rotated or not.
func (key *MasterKey) NeedsRotation() bool {
	return time.Since(key.CreationDate) > (time.Hour * 24 * 30 * 6)
}

// ToString converts the key to a string representation.
func (key *MasterKey) ToString() string {
	return fmt.Sprintf("%s/keys/%s/%s", key.VaultURL, key.Name, key.Version)
}

// ToMap converts the MasterKey to a map for serialization purposes.
func (key MasterKey) ToMap() map[string]interface{} {
	out := make(map[string]interface{})
	out["vaultUrl"] = key.VaultURL
	out["key"] = key.Name
	out["version"] = key.Version
	out["created_at"] = key.CreationDate.UTC().Format(time.RFC3339)
	out["enc"] = key.EncryptedKey
	return out
}

func decode(b []byte) ([]byte, error) {
	reader, enc := utfbom.Skip(bytes.NewReader(b))
	switch enc {
	case utfbom.UTF16LittleEndian:
		u16 := make([]uint16, (len(b)/2)-1)
		err := binary.Read(reader, binary.LittleEndian, &u16)
		if err != nil {
			return nil, err
		}
		return []byte(string(utf16.Decode(u16))), nil
	case utfbom.UTF16BigEndian:
		u16 := make([]uint16, (len(b)/2)-1)
		err := binary.Read(reader, binary.BigEndian, &u16)
		if err != nil {
			return nil, err
		}
		return []byte(string(utf16.Decode(u16))), nil
	}
	return ioutil.ReadAll(reader)
}
