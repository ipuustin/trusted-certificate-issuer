/*
Copyright 2021 Intel Coporation.
SPDX-License-Identifier: Apache-2.0
*/

package k8sutil

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/intel/trusted-certificate-issuer/api/v1alpha1"
	tcsapi "github.com/intel/trusted-certificate-issuer/api/v1alpha1"
	"github.com/intel/trusted-certificate-issuer/internal/tlsutil"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	namespaceEnvVar = "WATCH_NAMESPACE"
	namespaceFile   = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	TCSFinalizer    = "tcs.intel.com/issuer-protection"
)

// GetNamespace returns the namespace of the operator pod
func GetNamespace() string {
	ns := os.Getenv(namespaceEnvVar)
	if ns == "" {
		// If environment variable not set, give it a try to fetch it from
		// mounted filesystem by Kubernetes
		data, err := ioutil.ReadFile(namespaceFile)
		if err != nil {
			klog.Infof("Could not read namespace from %q: %v", namespaceFile, err)
		} else {
			ns = string(data)
		}
	}

	if ns == "" {
		ns = metav1.NamespaceDefault
	}

	return ns
}

func CreateCASecret(ctx context.Context, c client.Client, cert *x509.Certificate, name, ns string) error {
	if ns == "" {
		ns = GetNamespace()
	}
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Type: v1.SecretTypeTLS,
		Data: map[string][]byte{
			v1.TLSPrivateKeyKey: []byte(""),
			v1.TLSCertKey:       tlsutil.EncodeCert(cert),
		},
	}
	err := c.Create(ctx, secret)
	if err != nil && errors.IsAlreadyExists(err) {
		return c.Update(ctx, secret)
	}
	return err
}

func DeleteCASecret(ctx context.Context, c client.Client, name, ns string) error {
	if ns == "" {
		ns = GetNamespace()
	}
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
	}

	err := c.Delete(ctx, secret)
	if err != nil && errors.IsNotFound(err) {
		return nil
	}
	return err
}

func QuoteAttestationDeliver(
	ctx context.Context,
	c client.Client,
	instanceName, namespace string,
	requestType tcsapi.QuoteAttestationRequestType,
	signerNames []string,
	quote []byte,
	quotePubKey interface{},
	tokenLabel string) error {

	encPubKey, err := tlsutil.EncodePublicKey(quotePubKey)
	if err != nil {
		return err
	}

	encQuote := base64.StdEncoding.EncodeToString(quote)

	if namespace == "" {
		namespace = GetNamespace()
	}

	sgxAttestation := &tcsapi.QuoteAttestation{
		ObjectMeta: metav1.ObjectMeta{
			Name:       instanceName,
			Namespace:  namespace,
			Finalizers: []string{TCSFinalizer},
		},
		Spec: v1alpha1.QuoteAttestationSpec{
			Type:         requestType,
			Quote:        []byte(encQuote),
			QuoteVersion: tcsapi.ECDSAQuoteVersion3,
			SignerNames:  signerNames,
			ServiceID:    tokenLabel,
			PublicKey:    encPubKey,
		},
	}

	//Create a CR instance for QuoteAttestation
	//If not found object, return a new one
	err = c.Create(ctx, sgxAttestation)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			if err = c.Delete(ctx, sgxAttestation); err != nil {
				return fmt.Errorf("failed to delete existing QuoteAttestaion CR with name '%s'. Clear this before redeploy the operator: %v", instanceName, err)
			}

			err = c.Create(ctx, sgxAttestation)
		}
	}
	return err
}

func QuoteAttestationDelete(ctx context.Context, c client.Client, instanceName string, ns string) error {
	if ns == "" {
		ns = GetNamespace()
	}
	sgxAttestation := &tcsapi.QuoteAttestation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instanceName,
			Namespace: ns,
		},
	}

	if err := client.IgnoreNotFound(UnsetFinalizer(ctx, c, sgxAttestation)); err != nil {
		return fmt.Errorf("failed unset finalizer for '%s/%s': %v", ns, instanceName, err)
	}

	return client.IgnoreNotFound(c.Delete(ctx, sgxAttestation))
}

// Converts signer name to valid Kubernetes object name and nanespace
//  Ex:- intel.com/tcs -> tcs.intel.com, ""
///      tcsissuer.tcs.intel.com/sandbox.sgx-ca -> sgx-ca.tcs.intel.com, sandbox
//       tcsclusterissuer.tcs.intel.com/sgx-ca1 -> sgx-ca1.tcsclusterissuer.intel.tcs.com, ""
func SignerNameToResourceNameAndNamespace(signerName string) (string, string) {
	slices := strings.SplitN(signerName, "/", 2)
	if len(slices) == 2 {
		nameParts := strings.SplitN(slices[1], ".", 2)
		if len(nameParts) == 2 {
			return nameParts[1] + "." + slices[0], nameParts[0]
		}
		return slices[1] + "." + slices[0], ""
	}

	return slices[0], ""
}

func UnsetFinalizer(ctx context.Context, c client.Client, obj client.Object) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(2*time.Minute))
	defer cancel()
	key := client.ObjectKeyFromObject(obj)
	err := c.Get(timeoutCtx, key, obj)
	if err != nil {
		return err
	}

	list := obj.GetFinalizers()
	found := false
	for i, finalizer := range list {
		if finalizer == TCSFinalizer {
			found = true
			list = append(list[:i], list[i+1:]...)
			break
		}
	}

	if found {
		obj.SetFinalizers(list)
		timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(2*time.Minute))
		defer cancel()
		if err := client.IgnoreNotFound(c.Update(timeoutCtx, obj)); err != nil {
			return fmt.Errorf("failed to update finalizer (%v): %v", key, err)
		}
	}
	return nil
}
