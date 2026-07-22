package catalog_test

import (
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

func newDataRegistryService(t *testing.T) *catalog.DataRegistryService {
	t.Helper()
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	return catalog.NewDataRegistryService(catalog.NewDataCatalogAt(t.TempDir()), store)
}

func TestDataRegistry_RegisterGetList_RoundTrip(t *testing.T) {
	svc := newDataRegistryService(t)
	reg, err := svc.RegisterData(t.Context(), &nfv1.DataRegisterRequest{
		DataName:   "reference-genome",
		Version:    "hg38",
		Checksum:   "sha256:abc",
		StorageUri: "s3://reference/hg38.fa",
	})
	if err != nil {
		t.Fatalf("RegisterData: %v", err)
	}
	if reg.GetCasHash() == "" {
		t.Fatal("RegisterData returned an empty cas_hash")
	}
	got, err := svc.GetData(t.Context(), &nfv1.GetDataRequest{CasHash: reg.GetCasHash()})
	if err != nil {
		t.Fatalf("GetData: %v", err)
	}
	if got.GetDataName() != "reference-genome" {
		t.Fatalf("DataName = %q, want reference-genome", got.GetDataName())
	}
	listed, err := svc.ListData(t.Context(), &nfv1.ListDataRequest{})
	if err != nil {
		t.Fatalf("ListData: %v", err)
	}
	if len(listed.GetData()) != 1 || listed.GetData()[0].GetCasHash() != reg.GetCasHash() {
		t.Fatalf("ListData = %+v, want the registered data artifact", listed.GetData())
	}
}

func TestDataRegistry_GetData_ErrorContract(t *testing.T) {
	svc := newDataRegistryService(t)
	tests := []struct {
		name string
		req  *nfv1.GetDataRequest
		code codes.Code
	}{
		{name: "empty cas hash", req: &nfv1.GetDataRequest{}, code: codes.InvalidArgument},
		{name: "missing data", req: &nfv1.GetDataRequest{CasHash: "missing"}, code: codes.NotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.GetData(t.Context(), tt.req)
			if status.Code(err) != tt.code {
				t.Fatalf("GetData error = %v, want %s", err, tt.code)
			}
		})
	}
}

func TestDataRegistry_UninitializedDependencies_Unavailable(t *testing.T) {
	svc := catalog.NewDataRegistryService(nil, nil)
	tests := []struct {
		name string
		call func() error
	}{
		{name: "register", call: func() error {
			_, err := svc.RegisterData(t.Context(), &nfv1.DataRegisterRequest{
				DataName:   "reference",
				Checksum:   "sha256:test",
				StorageUri: "s3://test/artifact",
			})
			return err
		}},
		{name: "get", call: func() error {
			_, err := svc.GetData(t.Context(), &nfv1.GetDataRequest{CasHash: "abc"})
			return err
		}},
		{name: "list", call: func() error {
			_, err := svc.ListData(t.Context(), &nfv1.ListDataRequest{})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); status.Code(err) != codes.Unavailable {
				t.Fatalf("error = %v, want codes.Unavailable", err)
			}
		})
	}
}

func TestRegisterData_RequiredFields_InvalidArgument(t *testing.T) {
	svc := newDataRegistryService(t)
	tests := []struct {
		name string
		req  *nfv1.DataRegisterRequest
	}{
		{name: "data name", req: &nfv1.DataRegisterRequest{Checksum: "sha256:abc", StorageUri: "s3://ref/data"}},
		{name: "checksum", req: &nfv1.DataRegisterRequest{DataName: "reference", StorageUri: "s3://ref/data"}},
		{name: "storage uri", req: &nfv1.DataRegisterRequest{DataName: "reference", Checksum: "sha256:abc"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.RegisterData(t.Context(), tt.req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("RegisterData error = %v, want codes.InvalidArgument", err)
			}
		})
	}
}

func TestGetData_IndexPresentCASMissing_DataLoss(t *testing.T) {
	dir := t.TempDir()
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	svc := catalog.NewDataRegistryService(catalog.NewDataCatalogAt(dir), store)
	reg, err := svc.RegisterData(t.Context(), &nfv1.DataRegisterRequest{
		DataName:  "reference",
		Checksum:  "sha256:abc",
		StorageUri: "s3://ref/data",
	})
	if err != nil {
		t.Fatalf("RegisterData: %v", err)
	}
	if removeErr := os.Remove(filepath.Join(dir, reg.GetCasHash()+".datadefinition")); removeErr != nil {
		t.Fatalf("remove CAS object: %v", removeErr)
	}
	_, err = svc.GetData(t.Context(), &nfv1.GetDataRequest{CasHash: reg.GetCasHash()})
	if status.Code(err) != codes.DataLoss {
		t.Fatalf("GetData error = %v, want codes.DataLoss", err)
	}
}
