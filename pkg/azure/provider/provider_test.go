package provider

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v3"
	accesserrors "github.com/gardener/machine-controller-manager-provider-azure/pkg/azure/access/errors"
	"github.com/gardener/machine-controller-manager-provider-azure/pkg/azure/provider/helpers"
	"github.com/gardener/machine-controller-manager-provider-azure/pkg/azure/testhelp"
	"github.com/gardener/machine-controller-manager-provider-azure/pkg/azure/testhelp/fakes"
	"github.com/gardener/machine-controller-manager-provider-azure/pkg/azure/utils"
	"github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	"github.com/gardener/machine-controller-manager/pkg/util/provider/driver"
	"github.com/gardener/machine-controller-manager/pkg/util/provider/machinecodes/codes"
	"github.com/gardener/machine-controller-manager/pkg/util/provider/machinecodes/status"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
)

const (
	testResourceGroupName = "test-rg"
	testShootNs           = "test-shoot-ns"
	testWorkerPool0Name   = "test-worker-pool-0"
	testDataDiskName      = "test-data-disk"
)

func TestDeleteMachineWhenVMExists(t *testing.T) {
	table := []struct {
		description                string
		resourceGroup              string                  // used to build initial ClusterState
		existingVMNames            []string                // used to build initial ClusterState
		numDataDisks               int                     // used to build initial ClusterState
		cascadeDeleteOpts          fakes.CascadeDeleteOpts // used to build initial ClusterState
		machineClassResourceGroup  *string                 // for tests where a different resource Group than used to create ClusterState needs to be passed.
		targetVMNameToDelete       string                  // name of the VM that will be deleted via DeleteMachine
		shouldDeleteMachineSucceed bool
		checkClusterStateFn        func(g *WithT, ctx context.Context, factory fakes.Factory, vmName string)
	}{
		{
			"should delete all resources(NIC and OSDisk) when cascade delete is set for NIC and all Disks",
			testResourceGroupName,
			[]string{"vm-0", "vm-1"},
			1,
			fakes.CascadeDeleteAllResources,
			nil,
			"vm-1",
			true,
			func(g *WithT, ctx context.Context, factory fakes.Factory, vmName string) {
				checkClusterStateAndGetMachineResources(g, ctx, factory, vmName, false, false, false, true)
			},
		},
		{
			"should update VM before deleting the VM when cascade delete is not set for NIC but its set for disks",
			testResourceGroupName,
			[]string{"vm-0", "vm-1"},
			2,
			fakes.CascadeDeleteOpts{
				OSDisk:   to.Ptr(armcompute.DiskDeleteOptionTypesDelete),
				DataDisk: to.Ptr(armcompute.DiskDeleteOptionTypesDelete),
			},
			nil,
			"vm-0",
			true,
			func(g *WithT, ctx context.Context, factory fakes.Factory, vmName string) {
				checkClusterStateAndGetMachineResources(g, ctx, factory, vmName, false, false, false, true)
			},
		},
		{
			"should update VM before deleting the VM when cascade delete is not set for NIC and Disks",
			testResourceGroupName,
			[]string{"vm-0", "vm-1"},
			0,
			fakes.CascadeDeleteOpts{},
			nil,
			"vm-1",
			true,
			func(g *WithT, ctx context.Context, factory fakes.Factory, vmName string) {
				checkClusterStateAndGetMachineResources(g, ctx, factory, vmName, false, false, false, true)
			},
		},
		{
			"should skip delete if the resource group is not found",
			testResourceGroupName,
			[]string{"vm-0", "vm-1"},
			0,
			fakes.CascadeDeleteOpts{},
			to.Ptr("wrong-resource-group"),
			"vm-1",
			true,
			func(g *WithT, ctx context.Context, factory fakes.Factory, vmName string) {
				checkClusterStateAndGetMachineResources(g, ctx, factory, vmName, true, true, true, true)
			},
		},
	}

	g := NewWithT(t)
	ctx := context.Background()

	for _, entry := range table {
		t.Run(entry.description, func(t *testing.T) {
			// initialize cluster state
			//----------------------------------------------------------------------------
			// create provider spec
			providerSpecBuilder := testhelp.NewProviderSpecBuilder(entry.resourceGroup, testShootNs, testWorkerPool0Name).WithDefaultValues()
			if entry.numDataDisks > 0 {
				//Add data disks
				providerSpecBuilder.WithDataDisks(testDataDiskName, entry.numDataDisks)
			}
			providerSpec := providerSpecBuilder.Build()

			// create cluster state
			clusterState := fakes.NewClusterState(providerSpec)
			for _, vmName := range entry.existingVMNames {
				clusterState.AddMachineResources(fakes.NewMachineResourcesBuilder(providerSpec, vmName).WithCascadeDeleteOptions(entry.cascadeDeleteOpts).BuildAllResources())
			}
			// create fake factory
			fakeFactory := createDefaultFakeFactoryForMachineDelete(g, providerSpec.ResourceGroup, clusterState)

			// Create machine and machine class to be used to create DeleteMachineRequest
			machineClass, err := fakes.CreateMachineClass(providerSpec, entry.machineClassResourceGroup)
			g.Expect(err).To(BeNil())
			machine := &v1alpha1.Machine{
				ObjectMeta: fakes.NewMachineObjectMeta(testShootNs, entry.targetVMNameToDelete),
			}

			// Test environment before running actual test
			//----------------------------------------------------------------------------
			_, err = fakeFactory.VMAccess.Get(ctx, providerSpec.ResourceGroup, entry.targetVMNameToDelete, nil)
			g.Expect(err).To(BeNil())

			// Test
			//----------------------------------------------------------------------------
			testDriver := NewDefaultDriver(fakeFactory)
			_, err = testDriver.DeleteMachine(ctx, &driver.DeleteMachineRequest{
				Machine:      machine,
				MachineClass: machineClass,
				Secret:       fakes.CreateProviderSecret(),
			})
			g.Expect(err == nil).To(Equal(entry.shouldDeleteMachineSucceed))

			// evaluate cluster state post delete machine operation
			entry.checkClusterStateFn(g, ctx, *fakeFactory, entry.targetVMNameToDelete)
		})
	}
}

func TestDeleteMachineWhenVMDoesNotExist(t *testing.T) {
	const vmName = "test-vm-0"
	testVMID := fakes.CreateVirtualMachineID(testhelp.SubscriptionID, testResourceGroupName, vmName)

	table := []struct {
		description                string
		nicPresent                 bool
		osDiskPresent              bool
		numDataDisks               int
		vmID                       *string
		shouldDeleteMachineSucceed bool
		checkClusterStateFn        func(g *WithT, ctx context.Context, factory fakes.Factory, vmName string, dataDiskNames []string)
	}{
		{
			"should delete left over NIC and Disks when they are detached from VM",
			true, true, 1, nil, true,
			func(g *WithT, ctx context.Context, factory fakes.Factory, vmName string, dataDiskNames []string) {
				_, err := factory.DisksAccess.Get(ctx, testResourceGroupName, utils.CreateOSDiskName(vmName), nil)
				g.Expect(err).ToNot(BeNil())
				g.Expect(accesserrors.IsNotFoundAzAPIError(err)).To(BeTrue())
				for _, dataDiskName := range dataDiskNames {
					_, err := factory.DisksAccess.Get(ctx, testResourceGroupName, dataDiskName, nil)
					g.Expect(err).ToNot(BeNil())
					g.Expect(accesserrors.IsNotFoundAzAPIError(err)).To(BeTrue())
				}
				_, err = factory.InterfaceAccess.Get(ctx, testResourceGroupName, utils.CreateNICName(vmName), nil)
				g.Expect(err).ToNot(BeNil())
				g.Expect(accesserrors.IsNotFoundAzAPIError(err)).To(BeTrue())
			},
		},
		{
			"should fail delete of NIC when its still associated with a VM",
			true, false, 0, &testVMID, false,
			func(g *WithT, ctx context.Context, factory fakes.Factory, vmName string, dataDiskNames []string) {
				nic, err := factory.InterfaceAccess.Get(ctx, testResourceGroupName, utils.CreateNICName(vmName), nil)
				g.Expect(err).To(BeNil())
				g.Expect(nic.Properties.VirtualMachine).ToNot(BeNil())
				g.Expect(*nic.Properties.VirtualMachine.ID).To(Equal(testVMID))
			},
		},
		{
			"should fail delete of disks when its still associated with a VM",
			false, true, 1, &testVMID, false,
			func(g *WithT, ctx context.Context, factory fakes.Factory, vmName string, dataDiskNames []string) {
				osDisk, err := factory.DisksAccess.Get(ctx, testResourceGroupName, utils.CreateOSDiskName(vmName), nil)
				g.Expect(err).To(BeNil())
				g.Expect(osDisk.ManagedBy).ToNot(BeNil())
				g.Expect(*osDisk.ManagedBy).To(Equal(testVMID))
				for _, dataDiskName := range dataDiskNames {
					dataDisk, err := factory.DisksAccess.Get(ctx, testResourceGroupName, dataDiskName, nil)
					g.Expect(err).To(BeNil())
					g.Expect(*dataDisk.ManagedBy).ToNot(BeNil())
					g.Expect(*dataDisk.ManagedBy).To(Equal(testVMID))
				}
			},
		},
	}

	g := NewWithT(t)
	ctx := context.Background()

	for _, entry := range table {
		t.Run(entry.description, func(t *testing.T) {
			// initialize cluster state
			//----------------------------------------------------------------------------
			// create provider spec
			providerSpecBuilder := testhelp.NewProviderSpecBuilder(testResourceGroupName, testShootNs, testWorkerPool0Name).WithDefaultValues()
			if entry.numDataDisks > 0 {
				//Add data disks
				providerSpecBuilder.WithDataDisks(testDataDiskName, entry.numDataDisks)
			}
			providerSpec := providerSpecBuilder.Build()

			// create cluster state
			clusterState := fakes.NewClusterState(providerSpec)
			clusterState.AddMachineResources(fakes.NewMachineResourcesBuilder(providerSpec, vmName).BuildWith(false, entry.nicPresent, entry.osDiskPresent, entry.numDataDisks > 0, entry.vmID))

			// create fake factory
			fakeFactory := createDefaultFakeFactoryForMachineDelete(g, providerSpec.ResourceGroup, clusterState)

			// Create machine and machine class to be used to create DeleteMachineRequest
			machineClass, err := fakes.CreateMachineClass(providerSpec, to.Ptr(testResourceGroupName))
			g.Expect(err).To(BeNil())
			machine := &v1alpha1.Machine{
				ObjectMeta: fakes.NewMachineObjectMeta(testShootNs, vmName),
			}

			// Test
			//----------------------------------------------------------------------------
			testDriver := NewDefaultDriver(fakeFactory)
			_, err = testDriver.DeleteMachine(ctx, &driver.DeleteMachineRequest{
				Machine:      machine,
				MachineClass: machineClass,
				Secret:       fakes.CreateProviderSecret(),
			})
			g.Expect(err == nil).To(Equal(entry.shouldDeleteMachineSucceed))

			dataDiskNames := testhelp.CreateDataDiskNames(vmName, providerSpec)
			entry.checkClusterStateFn(g, ctx, *fakeFactory, vmName, dataDiskNames)
		})
	}
}

func TestDeleteMachineWithInducedErrors(t *testing.T) {
	const (
		testErrorCode = "test-error-code"
		vmName        = "test-vm-0"
	)

	testInternalServerError := testhelp.InternalServerError(testErrorCode)

	table := []struct {
		description               string
		vmAccessAPIBehaviorSpec   *fakes.APIBehaviorSpec
		rgAccessAPIBehaviorSpec   *fakes.APIBehaviorSpec
		diskAccessAPIBehaviorSpec *fakes.APIBehaviorSpec
		nicAccessAPIBehaviorSpec  *fakes.APIBehaviorSpec
		cascadeDeleteOpts         fakes.CascadeDeleteOpts
		vmExists                  bool
		checkErrorFn              func(g *WithT, err error)
		checkClusterStateFn       func(g *WithT, ctx context.Context, clusterState *fakes.ClusterState, vmName string)
	}{
		{
			"should fail when checking resource groups existence returns an error", nil,
			fakes.NewAPIBehaviorSpec().AddErrorResourceReaction(testResourceGroupName, testhelp.AccessMethodCheckExistence, testInternalServerError),
			nil, nil, fakes.CascadeDeleteAllResources, true, checkError,
			func(g *WithT, ctx context.Context, clusterState *fakes.ClusterState, vmName string) {
				createFactoryAndCheckClusterState(g, ctx, testResourceGroupName, clusterState, vmName, true, true, true)
			},
		},
		{
			"should fail when VM access Get call returns an error",
			fakes.NewAPIBehaviorSpec().AddErrorResourceReaction(vmName, testhelp.AccessMethodGet, testInternalServerError),
			nil, nil, nil, fakes.CascadeDeleteAllResources, true, checkError,
			func(g *WithT, ctx context.Context, clusterState *fakes.ClusterState, vmName string) {
				createFactoryAndCheckClusterState(g, ctx, testResourceGroupName, clusterState, vmName, true, true, true)
			},
		},
		{
			"non-existing-vm: should delete left over OSDisk even if error is returned when deleting left over NIC",
			nil, nil, nil,
			fakes.NewAPIBehaviorSpec().AddErrorResourceReaction(utils.CreateNICName(vmName), testhelp.AccessMethodBeginDelete, testInternalServerError),
			fakes.CascadeDeleteAllResources, false, checkError,
			func(g *WithT, ctx context.Context, clusterState *fakes.ClusterState, vmName string) {
				createFactoryAndCheckClusterState(g, ctx, testResourceGroupName, clusterState, vmName, false, true, false)
			},
		},
		{
			"non-existing-vm: should delete left over NIC even if there is a panic when deleting left over OSDisk",
			nil, nil,
			fakes.NewAPIBehaviorSpec().AddPanicResourceReaction(utils.CreateOSDiskName(vmName), testhelp.AccessMethodBeginDelete),
			nil, fakes.CascadeDeleteAllResources, false, checkError,
			func(g *WithT, ctx context.Context, clusterState *fakes.ClusterState, vmName string) {
				createFactoryAndCheckClusterState(g, ctx, testResourceGroupName, clusterState, vmName, false, false, true)
			},
		},
		{
			"should fail when existing VM's cascade delete options update returns an error",
			fakes.NewAPIBehaviorSpec().AddErrorResourceReaction(vmName, testhelp.AccessMethodBeginUpdate, testInternalServerError),
			nil, nil, nil, fakes.CascadeDeleteOpts{}, true, checkError,
			func(g *WithT, ctx context.Context, clusterState *fakes.ClusterState, vmName string) {
				createFactoryAndCheckClusterState(g, ctx, testResourceGroupName, clusterState, vmName, true, true, true)
			},
		},
		{
			"should fail when deletion of the VM post update of cascade deletion option completely fails",
			fakes.NewAPIBehaviorSpec().AddErrorResourceReaction(vmName, testhelp.AccessMethodBeginDelete, testInternalServerError),
			nil, nil, nil, fakes.CascadeDeleteOpts{}, true, checkError,
			func(g *WithT, ctx context.Context, clusterState *fakes.ClusterState, vmName string) {
				factory := createDefaultFakeFactoryForMachineDelete(g, testResourceGroupName, clusterState)
				machineResources := checkClusterStateAndGetMachineResources(g, ctx, *factory, vmName, true, true, true, true)
				// validate that the cascade delete options are now set
				g.Expect(machineResources.VM).ToNot(BeNil())
				checkCascadeDeleteOptions(t, *machineResources.VM, fakes.CascadeDeleteAllResources)
			},
		},
	}

	g := NewWithT(t)
	ctx := context.Background()
	// create provider spec
	providerSpec := testhelp.NewProviderSpecBuilder(testResourceGroupName, testShootNs, testWorkerPool0Name).WithDefaultValues().Build()

	for _, entry := range table {
		t.Run(entry.description, func(t *testing.T) {
			// initialize cluster state
			//----------------------------------------------------------------------------
			// create cluster state
			clusterState := fakes.NewClusterState(providerSpec)
			clusterState.AddMachineResources(fakes.NewMachineResourcesBuilder(providerSpec, vmName).WithCascadeDeleteOptions(entry.cascadeDeleteOpts).BuildWith(entry.vmExists, true, true, false, nil))

			// create fake factory
			fakeFactory := createFakeFactoryForDeleteMachineWithAPIBehaviorSpecs(g, providerSpec.ResourceGroup, clusterState, entry.rgAccessAPIBehaviorSpec, entry.vmAccessAPIBehaviorSpec, entry.diskAccessAPIBehaviorSpec, entry.nicAccessAPIBehaviorSpec)

			// Create machine and machine class to be used to create DeleteMachineRequest
			machineClass, err := fakes.CreateMachineClass(providerSpec, to.Ptr(testResourceGroupName))
			g.Expect(err).To(BeNil())
			machine := &v1alpha1.Machine{
				ObjectMeta: fakes.NewMachineObjectMeta(testShootNs, vmName),
			}
			// Test
			//----------------------------------------------------------------------------
			testDriver := NewDefaultDriver(fakeFactory)
			_, err = testDriver.DeleteMachine(ctx, &driver.DeleteMachineRequest{
				Machine:      machine,
				MachineClass: machineClass,
				Secret:       fakes.CreateProviderSecret(),
			})
			if entry.checkErrorFn != nil {
				entry.checkErrorFn(g, err)
			}
			if entry.checkClusterStateFn != nil {
				entry.checkClusterStateFn(g, ctx, clusterState, vmName)
			}
		})
	}
}

func TestDeleteMachineWhenProviderIsNotAzure(t *testing.T) {
	const vmName = "test-vm-0"
	g := NewWithT(t)
	ctx := context.Background()
	fakeFactory := fakes.NewFactory(testResourceGroupName)
	testDriver := NewDefaultDriver(fakeFactory)
	providerSpec := testhelp.NewProviderSpecBuilder(testResourceGroupName, testShootNs, testWorkerPool0Name).WithDefaultValues().Build()
	machineClass, err := fakes.CreateMachineClass(providerSpec, to.Ptr(testResourceGroupName))
	g.Expect(err).To(BeNil())
	machineClass.Provider = "aws" //set an incorrect provider
	machine := &v1alpha1.Machine{
		ObjectMeta: fakes.NewMachineObjectMeta(testShootNs, vmName),
	}
	_, err = testDriver.DeleteMachine(ctx, &driver.DeleteMachineRequest{
		Machine:      machine,
		MachineClass: machineClass,
		Secret:       fakes.CreateProviderSecret(),
	})
	g.Expect(err).ToNot(BeNil())
	var statusErr *status.Status
	g.Expect(errors.As(err, &statusErr)).Should(BeTrue())
	g.Expect(statusErr.Code()).To(Equal(codes.InvalidArgument))
}

func TestGetMachineStatus(t *testing.T) {
	table := []struct {
		description            string
		existingVMNames        []string
		targetVMName           string
		shouldOperationSucceed bool
		checkErrorFn           func(g *WithT, err error)
	}{
		{
			"should return an error for a non-existing VM", []string{"vm-0", "vm-1"}, "vm-2", false,
			func(g *WithT, err error) {
				var statusErr *status.Status
				g.Expect(err).ToNot(BeNil())
				g.Expect(errors.As(err, &statusErr)).Should(BeTrue())
				g.Expect(statusErr.Code()).To(Equal(codes.NotFound))
			},
		},
		{"should return a valid response for an existing VM", []string{"vm-0", "vm-1"}, "vm-0", true, nil},
	}

	g := NewWithT(t)
	ctx := context.Background()

	// create provider spec
	providerSpec := testhelp.NewProviderSpecBuilder(testResourceGroupName, testShootNs, testWorkerPool0Name).WithDefaultValues().Build()

	for _, entry := range table {
		t.Run(entry.description, func(t *testing.T) {
			// initialize cluster state
			//----------------------------------------------------------------------------
			// create cluster state
			clusterState := fakes.NewClusterState(providerSpec)
			for _, vmName := range entry.existingVMNames {
				clusterState.AddMachineResources(fakes.NewMachineResourcesBuilder(providerSpec, vmName).BuildAllResources())
			}
			// create fake factory
			fakeFactory := fakes.NewFactory(testResourceGroupName)
			vmAccess, err := fakeFactory.NewVirtualMachineAccessBuilder().WithClusterState(clusterState).Build()
			g.Expect(err).To(BeNil())
			fakeFactory.WithVirtualMachineAccess(vmAccess)

			// Create machine and machine class to be used to create DeleteMachineRequest
			machineClass, err := fakes.CreateMachineClass(providerSpec, to.Ptr(testResourceGroupName))
			g.Expect(err).To(BeNil())
			machine := &v1alpha1.Machine{
				ObjectMeta: fakes.NewMachineObjectMeta(testShootNs, entry.targetVMName),
			}

			// Test
			//----------------------------------------------------------------------------
			testDriver := NewDefaultDriver(fakeFactory)
			getMachineStatusResp, err := testDriver.GetMachineStatus(ctx, &driver.GetMachineStatusRequest{
				Machine:      machine,
				MachineClass: machineClass,
				Secret:       fakes.CreateProviderSecret(),
			})
			g.Expect(err == nil).To(Equal(entry.shouldOperationSucceed))
			if err == nil {
				g.Expect(getMachineStatusResp).ToNot(BeNil())
				g.Expect(getMachineStatusResp.NodeName).To(Equal(entry.targetVMName))
				instanceID := helpers.DeriveInstanceID(providerSpec.Location, entry.targetVMName)
				g.Expect(getMachineStatusResp.ProviderID).To(Equal(instanceID))
			}
			if entry.checkErrorFn != nil {
				entry.checkErrorFn(g, err)
			}
		})
	}
}

func TestListMachines(t *testing.T) {
	type machineResourcesTestSpec struct {
		vmName        string
		vmPresent     bool
		osDiskPresent bool
		nicPresent    bool
	}

	table := []struct {
		description     string
		mrTestSpecs     []machineResourcesTestSpec
		apiBehaviorSpec *fakes.APIBehaviorSpec
		expectedResult  []string
		expectedErr     bool
	}{
		{
			"should return no result if no resources exist", nil, nil, []string{}, false,
		},
		{
			"should return all vm names where vm's exist",
			[]machineResourcesTestSpec{
				{"vm-0", true, true, true},
				{"vm-1", true, true, true},
			}, nil, []string{"vm-0", "vm-1"}, false,
		},
		{
			"should return vm names only for vms which vm does not exist but a nic exists",
			[]machineResourcesTestSpec{
				{"vm-0", false, true, false},
				{"vm-1", false, false, true},
			}, nil, []string{"vm-1"}, false,
		},
	}

	g := NewWithT(t)
	ctx := context.Background()

	// create provider spec
	providerSpec := testhelp.NewProviderSpecBuilder(testResourceGroupName, testShootNs, testWorkerPool0Name).WithDefaultValues().Build()

	for _, entry := range table {
		t.Run(entry.description, func(t *testing.T) {
			// initialize cluster state
			//----------------------------------------------------------------------------
			// create cluster state
			clusterState := fakes.NewClusterState(providerSpec)
			if entry.mrTestSpecs != nil {
				for _, mrTestSpec := range entry.mrTestSpecs {
					var testVMID *string
					if !mrTestSpec.vmPresent {
						testVMID = to.Ptr(fakes.CreateVirtualMachineID(testhelp.SubscriptionID, testResourceGroupName, mrTestSpec.vmName))
					}
					clusterState.AddMachineResources(fakes.NewMachineResourcesBuilder(providerSpec, mrTestSpec.vmName).BuildWith(mrTestSpec.vmPresent, mrTestSpec.nicPresent, mrTestSpec.osDiskPresent, false, testVMID))
				}
			}

			// create fake factory
			fakeFactory := fakes.NewFactory(testResourceGroupName)
			resourceGraphAccess, err := fakeFactory.NewResourceGraphAccessBuilder().WithClusterState(clusterState).Build()
			g.Expect(err).To(BeNil())
			fakeFactory.WithResourceGraphAccess(resourceGraphAccess)

			// Create machine and machine class to be used to create DeleteMachineRequest
			machineClass, err := fakes.CreateMachineClass(providerSpec, to.Ptr(testResourceGroupName))
			g.Expect(err).To(BeNil())

			// Test
			//----------------------------------------------------------------------------
			testDriver := NewDefaultDriver(fakeFactory)
			listMachinesResp, err := testDriver.ListMachines(ctx, &driver.ListMachinesRequest{
				MachineClass: machineClass,
				Secret:       fakes.CreateProviderSecret(),
			})
			g.Expect(err != nil).To(Equal(entry.expectedErr))
			g.Expect(fakes.ActualSliceEqualsExpectedSlice(getVMNamesFromListMachineResponse(listMachinesResp), entry.expectedResult)).To(BeTrue())
		})
	}
}

func TestGetVolumeIDs(t *testing.T) {
	table := []struct {
		description                     string
		existingAzureDiskVolSourceNames []string
		existingAzureCSIVolHandles      []string
		existingNonAzureCSIVolHandles   []string
		expectedVolumeIDs               []string
	}{
		{"should return empty volumeIDs when no pv exist", nil, nil, nil, []string{}},
		{"should return empty volumeIDS when only non-csi vol sources are defined", nil, nil, []string{"non-az-csi-vol-1", "non-az-csi-vol-2"}, []string{}},
		{"should return azure disk vol sources when defined", []string{"az-disk-1", "az-disk-2"}, nil, []string{"non-az-csi-vol-1"}, []string{"az-disk-1", "az-disk-2"}},
		{"should return azure csi vol sources when defined", nil, []string{"az-csi-vol-1", "az-csi-vol-2"}, []string{"non-az-csi-vol-1"}, []string{"az-csi-vol-1", "az-csi-vol-2"}},
		{"should return azure disk and csi vol sources when defined", []string{"az-disk-1", "az-disk-2"}, []string{"az-csi-vol-1", "az-csi-vol-2"}, []string{"non-az-csi-vol-1"}, []string{"az-disk-1", "az-disk-2", "az-csi-vol-1", "az-csi-vol-2"}},
	}

	g := NewWithT(t)
	ctx := context.Background()
	for _, entry := range table {
		t.Run(entry.description, func(t *testing.T) {
			var pvSpecs []*corev1.PersistentVolumeSpec
			for _, diskVolSrcName := range entry.existingAzureDiskVolSourceNames {
				pvSpec := &corev1.PersistentVolumeSpec{
					PersistentVolumeSource: fakes.CreateAzureDiskPVSource(testResourceGroupName, diskVolSrcName),
				}
				pvSpecs = append(pvSpecs, pvSpec)
			}
			for _, azCSIVolHandle := range entry.existingAzureCSIVolHandles {
				pvSpec := &corev1.PersistentVolumeSpec{
					PersistentVolumeSource: fakes.CreateCSIPVSource(utils.AzureCSIDriverName, azCSIVolHandle),
				}
				pvSpecs = append(pvSpecs, pvSpec)
			}
			for _, nonAzCSIVolHandle := range entry.existingNonAzureCSIVolHandles {
				pvSpec := &corev1.PersistentVolumeSpec{
					PersistentVolumeSource: fakes.CreateCSIPVSource("test-non-az-driver", nonAzCSIVolHandle),
				}
				pvSpecs = append(pvSpecs, pvSpec)
			}
			testDriver := NewDefaultDriver(fakes.NewFactory(testResourceGroupName))
			resp, err := testDriver.GetVolumeIDs(ctx, &driver.GetVolumeIDsRequest{PVSpecs: pvSpecs})
			g.Expect(err).To(BeNil())
			g.Expect(fakes.ActualSliceEqualsExpectedSlice(resp.VolumeIDs, entry.expectedVolumeIDs))
		})
	}
}

/*
	List of tests:
 	1. Subnet is not there, should fail machine creation. No resource should be created.
	2. VM Image is not there, should fail machine creation. No resource should be created.
	3. VM Image is there with a mkt place plan but there is no agreement. No resource should be created.
	4. Update of agreement fails. No resource should be created.
	5. Getting VM Image failed. No resource should be created.
	6. Getting Subnet failed. No resource should be created.
	7.


	Use Subnet with a different resource group exists.
*/

// TestCreateMachineWhenPrerequisitesFail tests all cases where one or more Azure API calls made to get prerequisite
// resources fail.
func TestCreateMachineWhenPrerequisitesFail(t *testing.T) {
	const vmName = "vm-0"
	subnetName := fakes.CreateSubnetName(testShootNs)
	vnetName := testShootNs
	table := []struct {
		description                        string
		subnetName                         string
		vnetName                           string
		subnetResourceGroup                *string // If specified then this resource-group will be used to create a subnet resource in ClusterState
		providerSpecVnetResourceGroup      *string // If specified this will be used to set the vnet resource group in provider spec, else resource group at the providerSpec will be used.
		subnetExists                       bool
		vmImageExists                      bool
		agreementExists                    bool
		agreementAccepted                  bool
		vmAccessAPIBehavior                *fakes.APIBehaviorSpec
		subnetAccessAPIBehavior            *fakes.APIBehaviorSpec
		vmImageAccessAPIBehavior           *fakes.APIBehaviorSpec
		mktPlaceAgreementAccessAPIBehavior *fakes.APIBehaviorSpec
		checkMachineResourcesFn            func(g *WithT, ctx context.Context, factory fakes.Factory)
		checkErrorFn                       func(g *WithT, clusterState *fakes.ClusterState, err error)
	}{
		{
			"should fail machine creation when no subnet with given name exists, no resources should be created", subnetName, vnetName, nil, nil,
			false, true, true, true,
			nil, nil, nil, nil,
			func(g *WithT, ctx context.Context, factory fakes.Factory) {
				checkClusterStateAndGetMachineResources(g, ctx, factory, vmName, false, false, false, false)
			},
			func(g *WithT, clusterState *fakes.ClusterState, err error) {
				azRespErr := checkAndGetWrapperAzResponseError(g, err)
				g.Expect(azRespErr.StatusCode).To(Equal(http.StatusNotFound))
				g.Expect(azRespErr.ErrorCode).To(Equal(testhelp.ErrorCodeSubnetNotFound))
				g.Expect(azRespErr.RawResponse.Request.Method).To(Equal(http.MethodGet))
				g.Expect(fakes.IsSubnetURIPath(azRespErr.RawResponse.Request.URL.Path, testhelp.SubscriptionID, fakes.SubnetSpec{
					ResourceGroup: clusterState.ProviderSpec.ResourceGroup,
					SubnetName:    subnetName,
					VnetName:      vnetName,
				})).To(BeTrue())
			},
		},
		{
			"should fail machine creation when subnet GET fails, no resources should be created", subnetName, vnetName, nil, nil,
			true, true, true, true,
			nil, fakes.NewAPIBehaviorSpec().AddErrorResourceTypeReaction(fakes.SubnetResourceType, testhelp.AccessMethodGet, testhelp.InternalServerError("test-error-code")), nil, nil,
			func(g *WithT, ctx context.Context, factory fakes.Factory) {
				checkClusterStateAndGetMachineResources(g, ctx, factory, vmName, false, false, false, false)
			},
			func(g *WithT, clusterState *fakes.ClusterState, err error) {
				azRespErr := checkAndGetWrapperAzResponseError(g, err)
				g.Expect(azRespErr.StatusCode).To(Equal(http.StatusInternalServerError))
				g.Expect(azRespErr.ErrorCode).To(Equal("test-error-code"))
				g.Expect(azRespErr.RawResponse.Request.Method).To(Equal(http.MethodGet))
				g.Expect(fakes.IsSubnetURIPath(azRespErr.RawResponse.Request.URL.Path, testhelp.SubscriptionID, *clusterState.SubnetSpec)).To(BeTrue())
			},
		},
		{
			"should fail machine creation when resource group for subnet does not exist, no resources should be created", subnetName, vnetName, to.Ptr("vnet-rg"), to.Ptr("provider-spec-vnet-rg"),
			true, true, true, true,
			nil, nil, nil, nil,
			func(g *WithT, ctx context.Context, factory fakes.Factory) {
				checkClusterStateAndGetMachineResources(g, ctx, factory, vmName, false, false, false, false)
			},
			func(g *WithT, clusterState *fakes.ClusterState, err error) {
				azRespErr := checkAndGetWrapperAzResponseError(g, err)
				g.Expect(azRespErr.StatusCode).To(Equal(http.StatusNotFound))
				g.Expect(azRespErr.ErrorCode).To(Equal(testhelp.ErrorCodeResourceGroupNotFound))
				g.Expect(azRespErr.RawResponse.Request.Method).To(Equal(http.MethodGet))
				subnetSpec := *clusterState.SubnetSpec
				subnetSpec.ResourceGroup = "provider-spec-vnet-rg"
				g.Expect(fakes.IsSubnetURIPath(azRespErr.RawResponse.Request.URL.Path, testhelp.SubscriptionID, subnetSpec)).To(BeTrue())
			},
		},
		{
			"should fail machine creation when VM Image is not found, no resources should be created", subnetName, vnetName, nil, nil,
			true, false, false, false,
			nil, nil, nil, nil,
			func(g *WithT, ctx context.Context, factory fakes.Factory) {
				checkClusterStateAndGetMachineResources(g, ctx, factory, vmName, false, false, false, false)
			},
			func(g *WithT, clusterState *fakes.ClusterState, err error) {
				azRespErr := checkAndGetWrapperAzResponseError(g, err)
				g.Expect(azRespErr.StatusCode).To(Equal(http.StatusNotFound))
				g.Expect(azRespErr.ErrorCode).To(Equal(testhelp.ErrorCodeVMImageNotFound))
				g.Expect(azRespErr.RawResponse.Request.Method).To(Equal(http.MethodGet))
				publisher, offer, sku, version := fakes.GetDefaultVMImageParts()
				g.Expect(fakes.IsVMImageURIPath(azRespErr.RawResponse.Request.URL.Path, testhelp.SubscriptionID, clusterState.ProviderSpec.Location, fakes.VMImageSpec{
					Publisher: publisher,
					Offer:     offer,
					SKU:       sku,
					Version:   version,
				})).To(BeTrue())
			},
		},
	}

	g := NewWithT(t)
	ctx := context.Background()

	for _, entry := range table {
		t.Run(entry.description, func(t *testing.T) {
			// create provider spec
			providerSpecBuilder := testhelp.NewProviderSpecBuilder(testResourceGroupName, testShootNs, testWorkerPool0Name).WithDefaultValues()
			if entry.providerSpecVnetResourceGroup != nil {
				providerSpecBuilder.WithSubnetInfo(*entry.providerSpecVnetResourceGroup)
			}
			providerSpec := providerSpecBuilder.Build()

			// initialize cluster state
			//----------------------------------------------------------------------------
			// create cluster state
			clusterState := fakes.NewClusterState(providerSpec)
			if entry.vmImageExists {
				clusterState.WithDefaultVMImageSpec()
			}
			if entry.agreementExists {
				clusterState.WithAgreementTerms(entry.agreementAccepted)
			}
			if entry.subnetExists {
				vnetResourceGroup := providerSpec.ResourceGroup
				if entry.subnetResourceGroup != nil {
					vnetResourceGroup = *entry.subnetResourceGroup
				}
				clusterState.WithSubnet(vnetResourceGroup, entry.subnetName, entry.vnetName)
			}
			// create fake factory
			fakeFactory := createFakeFactoryForCreateMachineWithAPIBehaviorSpecs(g, providerSpec.ResourceGroup, clusterState, entry.vmAccessAPIBehavior, entry.subnetAccessAPIBehavior, entry.vmImageAccessAPIBehavior, entry.mktPlaceAgreementAccessAPIBehavior)

			// Create machine and machine class to be used to create DeleteMachineRequest
			machineClass, err := fakes.CreateMachineClass(providerSpec, to.Ptr(testResourceGroupName))
			g.Expect(err).To(BeNil())
			machine := &v1alpha1.Machine{
				ObjectMeta: fakes.NewMachineObjectMeta(testShootNs, vmName),
			}
			// Test
			//----------------------------------------------------------------------------
			testDriver := NewDefaultDriver(fakeFactory)
			resp, err := testDriver.CreateMachine(ctx, &driver.CreateMachineRequest{
				Machine:      machine,
				MachineClass: machineClass,
				Secret:       fakes.CreateProviderSecret(),
			})
			if err == nil {
				g.Expect(resp).ToNot(BeNil())
				g.Expect(resp.NodeName).To(Equal(vmName))
			}
			if entry.checkMachineResourcesFn != nil {
				entry.checkMachineResourcesFn(g, ctx, *fakeFactory)
			}
			if entry.checkErrorFn != nil {
				entry.checkErrorFn(g, clusterState, err)
			}
		})
	}
}

// unit test helper functions
//------------------------------------------------------------------------------------------------------

func checkError(g *WithT, err error) {
	var statusErr *status.Status
	g.Expect(errors.As(err, &statusErr)).To(BeTrue())
	g.Expect(statusErr.Code()).To(Equal(codes.Internal))
	// TODO: Add additional check when we improve status.Status error type to include the underline error as well.
}

func checkClusterStateAndGetMachineResources(g *WithT, ctx context.Context, factory fakes.Factory, vmName string, expectVMExists bool, expectNICExists bool, expectOSDiskExists bool, expectAssociatedVMID bool) fakes.MachineResources {
	vm := checkAndGetVM(g, ctx, factory, vmName, expectVMExists)
	nic := checkAndGetNIC(g, ctx, factory, vmName, expectNICExists, expectAssociatedVMID)
	osDisk := checkAndGetOSDisk(g, ctx, factory, vmName, expectOSDiskExists, expectAssociatedVMID)
	return fakes.MachineResources{
		Name:   vmName,
		VM:     vm,
		OSDisk: osDisk,
		NIC:    nic,
	}
}

func createFactoryAndCheckClusterState(g *WithT, ctx context.Context, resourceGroupName string, clusterState *fakes.ClusterState, vmName string, expectVMExists bool, expectNICExists bool, expectOSDiskExists bool) {
	factory := createDefaultFakeFactoryForMachineDelete(g, resourceGroupName, clusterState)
	checkClusterStateAndGetMachineResources(g, ctx, *factory, vmName, expectVMExists, expectNICExists, expectOSDiskExists, false)
}

func checkCascadeDeleteOptions(t *testing.T, vm armcompute.VirtualMachine, expectedCascadeDeleteOpts fakes.CascadeDeleteOpts) {
	g := NewWithT(t)
	if expectedCascadeDeleteOpts.NIC != nil {
		actualNICDeleteOpt := fakes.GetCascadeDeleteOptForNIC(vm)
		g.Expect(actualNICDeleteOpt).ToNot(BeNil())
		g.Expect(*actualNICDeleteOpt).To(Equal(*expectedCascadeDeleteOpts.NIC))
	}
	if expectedCascadeDeleteOpts.OSDisk != nil {
		actualOsDiskDeleteOpt := fakes.GetCascadeDeleteOptForOsDisk(vm)
		g.Expect(actualOsDiskDeleteOpt).ToNot(BeNil())
		g.Expect(*actualOsDiskDeleteOpt).To(Equal(*expectedCascadeDeleteOpts.OSDisk))
	}
	if expectedCascadeDeleteOpts.DataDisk != nil {
		deleteOpts := fakes.GetCascadeDeleteOptForDataDisks(vm)
		for dataDiskName, actualDeleteOpt := range deleteOpts {
			t.Logf("comparing disk delete option for data disk %s", dataDiskName)
			g.Expect(*actualDeleteOpt).To(Equal(*expectedCascadeDeleteOpts.DataDisk))
		}
	}
}

func checkAndGetVM(g *WithT, ctx context.Context, factory fakes.Factory, vmName string, expectVMExists bool) *armcompute.VirtualMachine {
	vmResp, err := factory.VMAccess.Get(ctx, testResourceGroupName, vmName, nil)
	if expectVMExists {
		g.Expect(err).To(BeNil())
		g.Expect(*vmResp.VirtualMachine.Name).To(Equal(vmName))
		return &vmResp.VirtualMachine
	} else {
		g.Expect(err).ToNot(BeNil())
		g.Expect(accesserrors.IsNotFoundAzAPIError(err)).To(BeTrue())
		return nil
	}
}

func checkAndGetNIC(g *WithT, ctx context.Context, factory fakes.Factory, vmName string, expectNICExists bool, expectAssociatedVMID bool) *armnetwork.Interface {
	nicResp, err := factory.InterfaceAccess.Get(ctx, testResourceGroupName, utils.CreateNICName(vmName), nil)
	if expectNICExists {
		g.Expect(err).To(BeNil())
		if expectAssociatedVMID {
			g.Expect(nicResp.Interface.Properties.VirtualMachine).ToNot(BeNil())
			g.Expect(nicResp.Interface.Properties.VirtualMachine.ID).ToNot(BeNil())
		}
		return &nicResp.Interface
	} else {
		g.Expect(err).ToNot(BeNil())
		g.Expect(accesserrors.IsNotFoundAzAPIError(err)).To(BeTrue())
		return nil
	}
}

func checkAndGetOSDisk(g *WithT, ctx context.Context, factory fakes.Factory, vmName string, expectOSDiskExists bool, expectAssociatedVMID bool) *armcompute.Disk {
	osDiskResp, err := factory.DisksAccess.Get(ctx, testResourceGroupName, utils.CreateOSDiskName(vmName), nil)
	if expectOSDiskExists {
		g.Expect(err).To(BeNil())
		if expectAssociatedVMID {
			g.Expect(osDiskResp.ManagedBy).ToNot(BeNil())
		}
		return &osDiskResp.Disk
	} else {
		g.Expect(err).ToNot(BeNil())
		g.Expect(accesserrors.IsNotFoundAzAPIError(err)).To(BeTrue())
		return nil
	}
}

func createDefaultFakeFactoryForMachineDelete(g *WithT, resourceGroup string, clusterState *fakes.ClusterState) *fakes.Factory {
	return createFakeFactoryForDeleteMachineWithAPIBehaviorSpecs(g, resourceGroup, clusterState, nil, nil, nil, nil)
}

func createFakeFactoryForDeleteMachineWithAPIBehaviorSpecs(g *WithT, resourceGroup string, clusterState *fakes.ClusterState,
	rgAccessAPIBehaviorSpec *fakes.APIBehaviorSpec,
	vmAccessAPIBehaviorSpec *fakes.APIBehaviorSpec,
	diskAccessAPIBehaviorSpec *fakes.APIBehaviorSpec,
	nicAccessAPIBehaviorSpec *fakes.APIBehaviorSpec) *fakes.Factory {

	factory := fakes.NewFactory(resourceGroup)
	vmAccess, err := factory.NewVirtualMachineAccessBuilder().WithClusterState(clusterState).WithAPIBehaviorSpec(vmAccessAPIBehaviorSpec).Build()
	g.Expect(err).To(BeNil())
	nicAccess, err := factory.NewNICAccessBuilder().WithClusterState(clusterState).WithAPIBehaviorSpec(nicAccessAPIBehaviorSpec).Build()
	g.Expect(err).To(BeNil())
	rgAccess, err := factory.NewResourceGroupsAccessBuilder().WithAPIBehaviorSpec(rgAccessAPIBehaviorSpec).Build()
	g.Expect(err).To(BeNil())
	diskAccess, err := factory.NewDiskAccessBuilder().WithClusterState(clusterState).WithAPIBehaviorSpec(diskAccessAPIBehaviorSpec).Build()
	g.Expect(err).To(BeNil())
	factory.
		WithVirtualMachineAccess(vmAccess).
		WithResourceGroupsAccess(rgAccess).
		WithNetworkInterfacesAccess(nicAccess).
		WithDisksAccess(diskAccess)

	return factory
}

func createFakeFactoryForCreateMachineWithAPIBehaviorSpecs(g *WithT, resourceGroup string, clusterState *fakes.ClusterState,
	vmAccessAPIBehaviorSpec *fakes.APIBehaviorSpec,
	subnetAccessAPIBehaviorSpec *fakes.APIBehaviorSpec,
	vmImageAccessAPIBehaviorSpec *fakes.APIBehaviorSpec,
	mktPlaceAgreementAccessAPIBehaviorSpec *fakes.APIBehaviorSpec) *fakes.Factory {

	factory := fakes.NewFactory(resourceGroup)
	vmAccess, err := factory.NewVirtualMachineAccessBuilder().WithClusterState(clusterState).WithAPIBehaviorSpec(vmAccessAPIBehaviorSpec).Build()
	g.Expect(err).To(BeNil())
	vmImageAccess, err := factory.NewImageAccessBuilder().WithClusterState(clusterState).WithAPIBehaviorSpec(vmImageAccessAPIBehaviorSpec).Build()
	g.Expect(err).To(BeNil())
	subnetAccess, err := factory.NewSubnetAccessBuilder().WithClusterState(clusterState).WithAPIBehaviorSpec(subnetAccessAPIBehaviorSpec).Build()
	g.Expect(err).To(BeNil())
	mktPlaceAgreementAccess, err := factory.NewMarketPlaceAgreementAccessBuilder().WithClusterState(clusterState).WithAPIBehaviorSpec(mktPlaceAgreementAccessAPIBehaviorSpec).Build()
	g.Expect(err).To(BeNil())
	nicAccess, err := factory.NewNICAccessBuilder().WithClusterState(clusterState).Build()
	g.Expect(err).To(BeNil())
	diskAccess, err := factory.NewDiskAccessBuilder().WithClusterState(clusterState).Build()
	g.Expect(err).To(BeNil())
	factory.
		WithVirtualMachineAccess(vmAccess).
		WithVirtualMachineImagesAccess(vmImageAccess).
		WithSubnetAccess(subnetAccess).
		WithMarketPlaceAgreementsAccess(mktPlaceAgreementAccess).
		WithNetworkInterfacesAccess(nicAccess).
		WithDisksAccess(diskAccess)

	return factory
}

func getVMNamesFromListMachineResponse(response *driver.ListMachinesResponse) []string {
	if response == nil {
		return []string{}
	}
	vmNames := make([]string, 0, len(response.MachineList))
	for _, vmName := range response.MachineList {
		vmNames = append(vmNames, vmName)
	}
	return vmNames
}

func checkAndGetWrapperAzResponseError(g *WithT, err error) *azcore.ResponseError {
	var statusErr *status.Status
	g.Expect(errors.As(err, &statusErr)).To(BeTrue())
	cause := statusErr.Cause()
	var azErr *azcore.ResponseError
	g.Expect(errors.As(cause, &azErr)).To(BeTrue())
	return azErr
}
