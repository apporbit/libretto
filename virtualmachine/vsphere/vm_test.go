// Copyright 2015 Apcera Inc. All rights reserved.

package vsphere

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/apcera/libretto/virtualmachine"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/nfc"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

type mockProgressReader struct {
	MockRead          func([]byte) (int, error)
	MockStartProgress func()
	MockWait          func()
}

func (r mockProgressReader) Read(p []byte) (n int, err error) {
	if r.MockRead != nil {
		return r.MockRead(p)
	}
	return 0, nil
}

func (r mockProgressReader) StartProgress() {
	if r.MockStartProgress != nil {
		r.MockStartProgress()
	}
}

func (r mockProgressReader) Wait() {
	if r.MockWait != nil {
		r.MockWait()
	}
}

type mockFinder struct {
	MockDatacenterList func(context.Context, string) ([]*object.Datacenter, error)
}

type mockCollector struct {
	MockRetrieveOne func(context.Context, types.ManagedObjectReference, []string, interface{}) error
}

type mockLease struct {
	MockLeaseProgress func(p int)
	MockWait          func() (*nfc.LeaseInfo, error)
	MockComplete      func() error
}

func (m mockLease) Progress(p int) {
	if m.MockLeaseProgress != nil {
		m.MockLeaseProgress(p)
	}
}

func (m mockLease) Complete() error {
	if m.MockComplete != nil {
		return m.MockComplete()
	}
	return nil
}

func (m mockLease) Wait() (*nfc.LeaseInfo, error) {
	if m.MockWait != nil {
		return m.MockWait()
	}
	return nil, nil
}

func (m mockCollector) RetrieveOne(c context.Context, mor types.ManagedObjectReference, ps []string, dst interface{}) error {
	if m.MockRetrieveOne != nil {
		return m.MockRetrieveOne(c, mor, ps, dst)
	}
	return nil
}

func (m mockFinder) DatacenterList(c context.Context, p string) ([]*object.Datacenter, error) {
	if m.MockDatacenterList != nil {
		return m.MockDatacenterList(c, p)
	}
	return []*object.Datacenter{}, nil
}

// Test that VM implements the VirtualMachine interface
func TestImplementation(t *testing.T) {
	var _ virtualmachine.VirtualMachine = (*VM)(nil)
}

// Test that getURI returns the correct URI
func TestGetURI(t *testing.T) {
	if uri := getURI("127.0.0.1"); uri != "https://127.0.0.1/sdk" {
		t.Fatalf("Invalid URI: %s", uri)
	}
}

func TestSetupSessionBadURI(t *testing.T) {
	var oldGetURI = getURI
	defer func() {
		getURI = oldGetURI
	}()
	getURI = func(string) string {
		return ""
	}
	vm := VM{}
	err := SetupSession(&vm)
	if _, ok := err.(ErrorParsingURL); !ok {
		t.Fatalf("Expected an error while parsing an invalid URI, got: %s", err)
	}
}

func TestSetupSessionClientError(t *testing.T) {
	var oldNewClient = newClient
	defer func() {
		newClient = oldNewClient
	}()
	vm := VM{
		Host:     "1.1.1.1",
		Username: "root",
		Password: "test",
	}
	newClient = func(vm *VM) (*govmomi.Client, error) {
		return nil, fmt.Errorf("error")
	}
	err := SetupSession(&vm)
	if _, ok := err.(ErrorClientFailed); !ok {
		t.Fatalf("Expected an error while connecting to the VI SDK, got: %s", err)
	}
}

func TestSetupSessionHappyPath(t *testing.T) {
	var oldNewClient = newClient
	var oldNewFinder = newFinder
	var oldNewCollector = newCollector
	defer func() {
		newClient = oldNewClient
		newFinder = oldNewFinder
		newCollector = oldNewCollector
	}()
	vm := VM{
		Host:     "1.1.1.1",
		Username: "root",
		Password: "test",
	}
	newClient = func(vm *VM) (*govmomi.Client, error) {
		return &govmomi.Client{}, nil
	}
	newFinder = func(c *vim25.Client) finder {
		return mockFinder{}
	}
	newCollector = func(c *vim25.Client) *property.Collector {
		return &property.Collector{}
	}

	err := SetupSession(&vm)
	if err != nil {
		t.Fatalf("Unexpected error setting up the VI SDK, got: %s", err)
	}
	if vm.ctx == nil {
		t.Fatalf("Context should not be nil")
	}
	if vm.cancel == nil {
		t.Fatalf("Cancel should not be nil")
	}
	if vm.client == nil {
		t.Fatalf("Client should not be nil")
	}
	if vm.finder == nil {
		t.Fatalf("Finder should not be nil")
	}
	if vm.collector == nil {
		t.Fatalf("Collector should not be nil")
	}
}

func TestGetDatacenterNoDatacenters(t *testing.T) {
	vm := &VM{
		Host:       "1.1.1.1",
		Username:   "root",
		Password:   "test",
		finder:     mockFinder{},
		Datacenter: "test-dc",
	}
	_, err := GetDatacenter(vm)
	if _, ok := err.(ErrorObjectNotFound); !ok {
		t.Fatalf("Expected to get an ErrorObjectNotFound got: %s", err)
	}
}

func TestGetDatacenterFinderError(t *testing.T) {
	f := mockFinder{}
	f.MockDatacenterList = func(context.Context, string) ([]*object.Datacenter, error) {
		return nil, errors.New("Failed to connect")
	}
	vm := &VM{
		Host:       "1.1.1.1",
		Username:   "root",
		Password:   "test",
		finder:     f,
		Datacenter: "test-dc",
	}
	_, err := GetDatacenter(vm)
	if _, ok := err.(ErrorObjectNotFound); !ok {
		t.Fatalf("Expected to get an ErrorObjectNotFound got: %s", err)
	}
}

func TestGetDatacenterPropertyError(t *testing.T) {
	f := mockFinder{}
	f.MockDatacenterList = func(context.Context, string) ([]*object.Datacenter, error) {
		return []*object.Datacenter{{}}, nil
	}
	c := mockCollector{}
	c.MockRetrieveOne = func(context.Context, types.ManagedObjectReference, []string, interface{}) error {
		return errors.New("failed to retrieve property")
	}
	vm := &VM{
		Host:       "1.1.1.1",
		Username:   "root",
		Password:   "test",
		finder:     f,
		Datacenter: "test-dc",
		collector:  c,
	}
	_, err := GetDatacenter(vm)
	if _, ok := err.(ErrorPropertyRetrieval); !ok {
		t.Fatalf("Expected to get a property retrieval error, got: %s", err)
	}
}

func TestGetDatacenterHappyPath(t *testing.T) {
	f := mockFinder{}
	f.MockDatacenterList = func(context.Context, string) ([]*object.Datacenter, error) {
		return []*object.Datacenter{{}}, nil
	}
	c := mockCollector{}
	c.MockRetrieveOne = func(_ context.Context, _ types.ManagedObjectReference, _ []string, dst interface{}) error {
		dc := dst.(*mo.Datacenter)
		dc.Name = "test-dc"
		return nil
	}
	vm := &VM{
		Host:       "1.1.1.1",
		Username:   "root",
		Password:   "test",
		finder:     f,
		Datacenter: "test-dc",
		collector:  c,
	}
	dc, err := GetDatacenter(vm)
	if err != nil {
		t.Fatalf("Expected to get no errors, got: %s", err)
	}
	if dc.Name != "test-dc" {
		t.Fatalf("Expected to get a dc managed object with the correct name, got: %+v", dc)
	}
}

func TestParseOvfOpenFileError(t *testing.T) {
	var oldOpen = open
	defer func() {
		open = oldOpen
	}()

	open = func(name string) (file *os.File, err error) {
		return nil, fmt.Errorf("Failed to open file")
	}
	_, err := parseOvf("foo")
	if err == nil {
		t.Fatalf("Expected an error when the function can't open an ovf file got nil")
	}
}

func TestParseOvfReadError(t *testing.T) {
	var oldOpen = open
	var oldReadAll = readAll
	defer func() {
		open = oldOpen
		readAll = oldReadAll
	}()

	open = func(name string) (file *os.File, err error) {
		return &os.File{}, nil
	}
	readAll = func(io.Reader) ([]byte, error) {
		return nil, fmt.Errorf("Error reading file")
	}
	_, err := parseOvf("foo")
	if err == nil {
		t.Fatalf("Expected an error when the function can't read an ovf file got nil")
	}
}

func TestParseOvfHappyPath(t *testing.T) {
	var oldOpen = open
	var oldReadAll = readAll
	defer func() {
		open = oldOpen
		readAll = oldReadAll
	}()

	open = func(name string) (file *os.File, err error) {
		return &os.File{}, nil
	}
	readAll = func(io.Reader) ([]byte, error) {
		return []byte("test bytes"), nil
	}
	b, err := parseOvf("foo")
	if err != nil {
		t.Fatalf("Expected no error, got: %s", err)
	}
	if string(b) != "test bytes" {
		t.Fatalf("Expected 'test bytes' got: %s", string(b))
	}
}

func TestGetDatastoreNoDatastores(t *testing.T) {
	vm := &VM{
		Host:       "1.1.1.1",
		Username:   "root",
		Password:   "test",
		finder:     mockFinder{},
		Datastores: []string{"test"},
	}
	_, err := findDatastore(vm, &mo.Datacenter{}, "test")
	if _, ok := err.(ErrorObjectNotFound); !ok {
		t.Fatalf("Expected to get an ErrorObjectNotFound got: %s", err)
	}
}

func TestGetDatastorePropertyError(t *testing.T) {
	c := mockCollector{}
	c.MockRetrieveOne = func(context.Context, types.ManagedObjectReference, []string, interface{}) error {
		return errors.New("failed to retrieve property")
	}
	// Create a datacenter with one datastore
	dc := mo.Datacenter{Datastore: []types.ManagedObjectReference{{}}}
	vm := &VM{
		Host:       "1.1.1.1",
		Username:   "root",
		Password:   "test",
		Datacenter: "test-dc",
		collector:  c,
	}
	_, err := findDatastore(vm, &dc, "test")
	if _, ok := err.(ErrorPropertyRetrieval); !ok {
		t.Fatalf("Expected to get a property retrieval error, got: %s", err)
	}
}

func TestGetDatastoreHappyPath(t *testing.T) {
	c := mockCollector{}
	c.MockRetrieveOne = func(c context.Context, t types.ManagedObjectReference, ps []string, dst interface{}) error {
		ds := dst.(*mo.Datastore)
		ds.Name = "test-datastore"
		return nil
	}
	// Create a datacenter with one datastore
	dc := mo.Datacenter{Datastore: []types.ManagedObjectReference{{}}}
	vm := &VM{
		Host:       "1.1.1.1",
		Username:   "root",
		Password:   "test",
		Datacenter: "test-dc",
		collector:  c,
	}
	ds, err := findDatastore(vm, &dc, "test-datastore")
	if err != nil {
		t.Fatalf("Expected no error got : %s", err)
	}
	if ds.Name != "test-datastore" {
		t.Fatalf("Expected to get a datstore with the name 'test-datastore' got: %s", ds.Name)
	}
}

func TestFindComputeResourceError(t *testing.T) {
	var oldFindMob = findMob
	defer func() {
		findMob = oldFindMob
	}()
	expectedError := "Error finding mob"
	findMob = func(vm *VM, mor types.ManagedObjectReference, name string) (*types.ManagedObjectReference, error) {
		return nil, fmt.Errorf(expectedError)
	}

	vm := &VM{
		Host:     "1.1.1.1",
		Username: "root",
		Password: "test",
	}
	_, err := findComputeResource(vm, &mo.Datacenter{}, "test")
	if err.Error() != expectedError {
		t.Fatalf("Expected to get an error, got: %s", err)
	}
}

func TestFindComputeResourceHappyPath(t *testing.T) {
	c := mockCollector{}
	c.MockRetrieveOne = func(c context.Context, t types.ManagedObjectReference, ps []string, dst interface{}) error {
		ds := dst.(*mo.ComputeResource)
		ds.Name = "test"
		return nil
	}
	var oldFindMob = findMob
	defer func() {
		findMob = oldFindMob
	}()
	findMob = func(vm *VM, mor types.ManagedObjectReference, name string) (*types.ManagedObjectReference, error) {
		return &types.ManagedObjectReference{}, nil
	}
	vm := &VM{
		Host:      "1.1.1.1",
		Username:  "root",
		Password:  "test",
		collector: c,
	}
	cr, err := findComputeResource(vm, &mo.Datacenter{}, "test")
	if err != nil {
		t.Fatalf("Expected no error got : %s", err)
	}
	if cr.Name != "test" {
		t.Fatalf("Expected to get a cr with the name 'test' got: %s", cr.Name)
	}
}

func TestFindComputeResourcePropertyError(t *testing.T) {
	c := mockCollector{}
	c.MockRetrieveOne = func(c context.Context, t types.ManagedObjectReference, ps []string, dst interface{}) error {
		return fmt.Errorf("Failed to retrieve property")
	}
	var oldFindMob = findMob
	defer func() {
		findMob = oldFindMob
	}()
	findMob = func(vm *VM, mor types.ManagedObjectReference, name string) (*types.ManagedObjectReference, error) {
		return &types.ManagedObjectReference{}, nil
	}
	vm := &VM{
		Host:      "1.1.1.1",
		Username:  "root",
		Password:  "test",
		collector: c,
	}
	cr, err := findComputeResource(vm, &mo.Datacenter{}, "test")
	if err == nil || cr != nil {
		t.Fatalf("Expected to get an err got nil. Expected cr to be nil, got : %+v", cr)
	}
}

func TestCreateNetworkMapping(t *testing.T) {
	nwMap := map[string]string{
		"nw1": "mapping1",
		"nw3": "mapping2",
	}
	networkMors := []types.ManagedObjectReference{{Type: "Network"}, {Type: "Network"}}
	callCount := 1 //First call
	c := mockCollector{}
	c.MockRetrieveOne = func(c context.Context, t types.ManagedObjectReference, ps []string, dst interface{}) error {
		nw := dst.(*mo.Network)
		if callCount == 1 {
			nw.Name = "mapping1"
		} else {
			nw.Name = "mapping2"
		}
		callCount++
		return nil
	}
	vm := &VM{
		Host:      "1.1.1.1",
		Username:  "root",
		Password:  "test",
		collector: c,
	}
	mappings, err := createNetworkMapping(vm, nwMap, networkMors)
	if err != nil {
		t.Fatalf("Expected to a nil err. Got: %s", err)
	}
	if len(mappings) != 2 {
		t.Fatalf("Expected to get 2 mappings. Got: %s", mappings)
	}
}

func TestCreateNetworkMappingPropertyFailed(t *testing.T) {
	nwMap := map[string]string{
		"nw1": "mapping1",
		"nw3": "mapping2",
	}
	networkMors := []types.ManagedObjectReference{{Type: "Network"}, {Type: "Network"}}
	c := mockCollector{}
	expectedError := "failed to retrieve property"
	c.MockRetrieveOne = func(c context.Context, t types.ManagedObjectReference, ps []string, dst interface{}) error {
		return fmt.Errorf(expectedError)
	}
	vm := &VM{
		Host:      "1.1.1.1",
		Username:  "root",
		Password:  "test",
		collector: c,
	}
	_, err := createNetworkMapping(vm, nwMap, networkMors)
	if err.Error() != expectedError {
		t.Fatalf("Expected to get err %s, got: %s", expectedError, err)
	}
}

func TestCreateObjectError(t *testing.T) {
	nwMap := map[string]string{
		"nw1": "mapping1",
		"nw3": "mapping2",
	}
	networkMors := []types.ManagedObjectReference{{Type: "Network"}, {Type: "Network"}}
	c := mockCollector{}
	c.MockRetrieveOne = func(c context.Context, t types.ManagedObjectReference, ps []string, dst interface{}) error {
		nw := dst.(*mo.Network)
		nw.Name = "mapping1"
		return nil
	}
	vm := &VM{
		Host:      "1.1.1.1",
		Username:  "root",
		Password:  "test",
		collector: c,
	}
	_, err := createNetworkMapping(vm, nwMap, networkMors)
	if _, ok := err.(ErrorObjectNotFound); !ok {
		t.Fatalf("Expected to get an ErrorObjectNotFound got: %s", err)
	}
}

func TestResetUnitNumbers(t *testing.T) {
	spec := types.OvfCreateImportSpecResult{}
	vmSpec := &types.VirtualMachineImportSpec{}
	vmSpec.ConfigSpec.DeviceChange = []types.BaseVirtualDeviceConfigSpec{
		&types.VirtualDeviceConfigSpec{
			Device: &types.VirtualDevice{
				UnitNumber: new(int32),
			},
		},
	}
	spec.ImportSpec = vmSpec
	resetUnitNumbers(&spec)
	s := &spec.ImportSpec.(*types.VirtualMachineImportSpec).ConfigSpec
	// Should not add any device
	if len(s.DeviceChange) != 1 {
		t.Fatalf("Expected only one device, got: %d", len(s.DeviceChange))
	}
	if n := s.DeviceChange[0].GetVirtualDeviceConfigSpec().Device.GetVirtualDevice().UnitNumber; *n != -1 {
		t.Fatalf("Expected to get -1 for the unit number, got: %d", n)
	}
}

func TestUploadOvfLeaseWaitError(t *testing.T) {
	l := mockLease{
		MockWait: func() (*nfc.LeaseInfo, error) {
			return nil, fmt.Errorf("Error waiting on the nfc lease")
		},
	}
	vm := VM{}
	sr := types.OvfCreateImportSpecResult{}
	err := uploadOvf(&vm, &sr, l)
	if err == nil {
		t.Fatalf("Expected to get an error, got: %s", err)
	}
}

func TestCreateRequestNewRequestError(t *testing.T) {
	errProtocol := `unsupported protocol scheme ""`
	err := createRequest(mockProgressReader{}, "foo", true, 0, "", "foo")
	if !strings.Contains(err.Error(), errProtocol) {
		t.Fatalf("Expected error to contain %q, got: %q", errProtocol, err)
	}
}

func TestCreateRequestBadStatusCode(t *testing.T) {
	var oldClientDo = clientDo
	defer func() {
		clientDo = oldClientDo
	}()
	clientDo = func(c *http.Client, r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404}, nil
	}
	err := createRequest(mockProgressReader{}, "foo", true, 0, "", "foo")
	if _, ok := err.(ErrorBadResponse); !ok {
		t.Fatalf("Expected to get a bad response error got: %s", err)
	}
}

func TestCreateRequestHappyPath(t *testing.T) {
	var oldClientDo = clientDo
	defer func() {
		clientDo = oldClientDo
	}()
	clientDo = func(c *http.Client, r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 201}, nil
	}
	err := createRequest(mockProgressReader{}, "foo", true, 0, "", "foo")
	if err != nil {
		t.Fatalf("Expected to get no errors got: %s", err)
	}
}

var vmMo = &mo.VirtualMachine{
	Runtime: types.VirtualMachineRuntimeInfo{
		Question: &types.VirtualMachineQuestionInfo{
			Text: "test question",
			Choice: types.ChoiceOption{
				ChoiceInfo: []types.BaseElementDescription{
					&types.ElementDescription{
						Key:         "0",
						Description: types.Description{Summary: "Zero"},
					},
					&types.ElementDescription{
						Key:         "1",
						Description: types.Description{Summary: "One"},
					},
				},
			},
		},
	},
}

func TestAnswerQuestions_HappyPath(t *testing.T) {
	var oldAnswerQuestion = answerVSphereQuestion
	defer func() {
		answerVSphereQuestion = oldAnswerQuestion
	}()

	answerVSphereQuestion = func(vm *VM, vmMo *mo.VirtualMachine, questionId, answer string) error {
		return nil
	}
	testCases := []struct {
		key         string
		expectError bool
	}{
		{"test", false},
		{"foo", false},
		{"[", true},
		{"", false},
	}
	for _, tc := range testCases {
		vm := VM{}
		if tc.key != "" {
			vm.QuestionResponses = map[string]string{tc.key: "foo"}
		}
		err := vm.answerQuestion(vmMo)
		if err == nil && tc.expectError {
			t.Fatalf("Expected an error due to regexp compliation, got nil")
		}
		if err != nil {
			_, re := regexp.Compile(tc.key)
			if !strings.Contains(err.Error(), re.Error()) {
				t.Errorf("Expected error due to regexp compilation; got: %s", err)
			}
		}
	}
}

func TestAnswerQuestions_ErrorPath(t *testing.T) {
	var oldAnswerQuestion = answerVSphereQuestion
	defer func() {
		answerVSphereQuestion = oldAnswerQuestion
	}()

	answerVSphereQuestion = func(vm *VM, vmMo *mo.VirtualMachine, questionId, answer string) error {
		return fmt.Errorf("Simulated error")
	}
	testCases := []struct {
		key         string
		expectError bool
	}{
		{"test", true},
		{"", false},
		{"foo", false},
	}
	for _, tc := range testCases {
		vm := VM{}
		if tc.key != "" {
			vm.QuestionResponses = map[string]string{tc.key: "foo"}
		}
		err := vm.answerQuestion(vmMo)
		if err == nil && tc.expectError {
			t.Fatalf("Expected an error, got nil")
		}
	}
}

func TestResolveAnswerAndOptions(t *testing.T) {
	testCases := []struct {
		answer         string
		expectedAnswer string
	}{
		{"0", "0"},
		{"1", "1"},
		{"One", "1"},
		{"zEro", "0"},
		{"nomatch", "nomatch"},
	}
	q := vmMo.Runtime.Question
	expectedValidOptions := "(0) Zero (1) One"
	for _, tc := range testCases {
		newAnswer, validOptions := resolveAnswerAndOptions(q.Choice.ChoiceInfo, tc.answer)
		if tc.expectedAnswer != newAnswer {
			t.Errorf("Expected answer %q to resolve to %q; got %q", tc.answer, tc.expectedAnswer, newAnswer)
		}
		if expectedValidOptions != validOptions {
			t.Errorf("Expected validOptions %q; got %q", expectedValidOptions, validOptions)
		}
	}
}
