package sso

import "encoding/xml"

type SAMLServiceProvider struct {
	EntityID string
	ACSURL   string
}

func NewSAMLServiceProvider(entityID, acsURL string) *SAMLServiceProvider {
	return &SAMLServiceProvider{EntityID: entityID, ACSURL: acsURL}
}

func (s *SAMLServiceProvider) GenerateMetadata() string {
	type nameIDFormat struct {
		Value string `xml:",chardata"`
	}
	type assertionConsumerService struct {
		Binding   string `xml:"Binding,attr"`
		Location  string `xml:"Location,attr"`
		Index     int    `xml:"index,attr"`
		IsDefault bool   `xml:"isDefault,attr"`
	}
	type spSSODescriptor struct {
		ProtocolSupportEnumeration string                   `xml:"protocolSupportEnumeration,attr"`
		NameIDFormat               nameIDFormat             `xml:"md:NameIDFormat"`
		AssertionConsumerService   assertionConsumerService `xml:"md:AssertionConsumerService"`
	}
	type descriptor struct {
		XMLName         xml.Name        `xml:"md:EntityDescriptor"`
		Namespace       string          `xml:"xmlns:md,attr"`
		EntityID        string          `xml:"entityID,attr"`
		SPSSODescriptor spSSODescriptor `xml:"md:SPSSODescriptor"`
	}
	metadata := descriptor{
		Namespace: "urn:oasis:names:tc:SAML:2.0:metadata",
		EntityID:  s.EntityID,
		SPSSODescriptor: spSSODescriptor{
			ProtocolSupportEnumeration: "urn:oasis:names:tc:SAML:2.0:protocol",
			NameIDFormat:               nameIDFormat{Value: "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress"},
			AssertionConsumerService:   assertionConsumerService{Binding: "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST", Location: s.ACSURL, Index: 1, IsDefault: true},
		},
	}
	b, err := xml.Marshal(metadata)
	if err != nil {
		return ""
	}
	return xml.Header + string(b)
}
