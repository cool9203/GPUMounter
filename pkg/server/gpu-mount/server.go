package gpu_mount

import (
	gpu_mount "GPUMounter/pkg/api/gpu-mount"
	"GPUMounter/pkg/config"
	"GPUMounter/pkg/util"
	"GPUMounter/pkg/util/gpu"
	"GPUMounter/pkg/util/gpu/allocator"
	. "GPUMounter/pkg/util/log"
	"context"
	"errors"
	"os"

	k8s_error "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type GPUMountImpl struct {
	*allocator.GPUAllocator
}

func NewGPUMounter() (*GPUMountImpl, error) {
	Logger.Info("Creating gpu mounter")
	gpuMounter := &GPUMountImpl{}
	tmp, err := allocator.NewGPUAllocator()
	if err != nil {
		Logger.Error("Failed to init gpu allocator")
		return nil, err
	}
	Logger.Info("Successfully created gpu allocator")
	gpuMounter.GPUAllocator = tmp
	return gpuMounter, nil
}

func (gpuMountImpl GPUMountImpl) AddGPU(_ context.Context, request *gpu_mount.AddGPURequest) (*gpu_mount.AddGPUResponse, error) {
	Logger.Info("AddGPU Service Called")
	Logger.Info("request: ", request)

	clientset, err := config.GetClientSet()
	if err != nil {
		Logger.Error("Connect to k8s failed")
		return nil, errors.New("Service Internal Error ")
	}
	targetPod, err := clientset.CoreV1().Pods(request.Namespace).Get(context.TODO(), request.PodName, metav1.GetOptions{})
	if err != nil {
		if k8s_error.IsNotFound(err) {
			Logger.Error("No such Pod: " + request.PodName + " in Namepsace: " + request.Namespace)
			Logger.Error(err)
			return &gpu_mount.AddGPUResponse{AddGpuResult: gpu_mount.AddGPUResponse_PodNotFound}, nil
		} else {
			Logger.Error("Get Pod: " + request.PodName + " in Namespace: " + request.Namespace + " failed")
			Logger.Error(err)
			return nil, errors.New("Service Internal Error ")
		}
	}
	Logger.Info("Successfully get Pod: " + request.Namespace + " in cluster")

	if !util.CanMount(gpuMountImpl.GetMountType(targetPod), request) {
		return nil, errors.New(gpu.FailedCreated)
	}

	gpuNum := int(request.GpuNum)
	gpuNumPerPod := 1
	if request.IsEntireMount {
		gpuNumPerPod = gpuNum
	}
	gpuResources, err := gpuMountImpl.GetAvailableGPU(targetPod, gpuNum, gpuNumPerPod)

	if err != nil {
		if err.Error() == gpu.InsufficientGPU {
			Logger.Error("Insufficient gpu for Pod: ", targetPod.Name, " Namespace: "+targetPod.Namespace)
			return &gpu_mount.AddGPUResponse{AddGpuResult: gpu_mount.AddGPUResponse_InsufficientGPU}, nil
		} else if err.Error() == gpu.FailedCreated {
			Logger.Error("Failed to create slave pod for Pod: ", targetPod.Name, " Namespace: "+targetPod.Namespace)
			return nil, errors.New("Service Internal Error ")
		}
		Logger.Error("Can not get available gpu")
		return nil, errors.New("Service Internal Error ")
	}

	for idx, targetGPU := range gpuResources {
		Logger.Info("Start mounting, Total: ", gpuNum, " Current: ", idx+1)
		err = util.MountGPU(targetPod, targetGPU)
		if err != nil {
			Logger.Error("Mount GPU: " + targetGPU.String() + " to Pod: " + request.PodName + " in Namespace: " + request.Namespace + " failed")
			Logger.Error(err)
			for _, freeGPU := range gpuResources {
				err = clientset.CoreV1().Pods(os.Getenv("GPU_POOL_NAMESPACE")).Delete(context.TODO(), freeGPU.PodName, *metav1.NewDeleteOptions(0))
				if err != nil {
					Logger.Error("Failed to release GPU: ", freeGPU.String())
				}
			}
			return nil, errors.New("Service Internal Error ")
		}
		Logger.Info("Mount GPU: " + targetGPU.String() + " to Pod: " + request.PodName + " in Namespace: " + request.Namespace + " successfully")
	}

	Logger.Info("Successfully mount all GPU to Pod: " + request.PodName + " in Namespace: " + request.Namespace)
	return &gpu_mount.AddGPUResponse{AddGpuResult: gpu_mount.AddGPUResponse_Success}, nil
}

func (gpuMountImpl GPUMountImpl) RemoveGPU(_ context.Context, request *gpu_mount.RemoveGPURequest) (*gpu_mount.RemoveGPUResponse, error) {
	Logger.Info("RemoveGPU Service Called")
	Logger.Info("request: ", request)

	clientset, err := config.GetClientSet()
	if err != nil {
		Logger.Error("Connect to k8s failed")
		return nil, errors.New("Service Internal Error ")
	}
	targetPod, err := clientset.CoreV1().Pods(request.Namespace).Get(context.TODO(), request.PodName, metav1.GetOptions{})
	if err != nil {
		if k8s_error.IsNotFound(err) {
			Logger.Error("No such Pod: " + request.PodName + " in Namepsace: " + request.Namespace)
			Logger.Error(err)
			return &gpu_mount.RemoveGPUResponse{RemoveGpuResult: gpu_mount.RemoveGPUResponse_PodNotFound}, nil
		} else {
			Logger.Error("Get Pod: " + request.PodName + " in Namespace: " + request.Namespace + " failed")
			Logger.Error(err)
			return nil, errors.New("Service Internal Error ")
		}
	}
	Logger.Info("Successfully get Pod: ", request.PodName, "in Namespace: ", request.Namespace)

	removeGPUs, err := gpuMountImpl.GetRemoveGPU(targetPod, request.Uuids)
	if err != nil {
		Logger.Error("Failed to get remove gpu of Pod: ", targetPod.Name)
		Logger.Error(err)
		return nil, err
	}
	if len(removeGPUs) == 0 {
		Logger.Error("Invalid UUIDs: ", request.Uuids)
		return &gpu_mount.RemoveGPUResponse{
			RemoveGpuResult: gpu_mount.RemoveGPUResponse_GPUNotFound,
		}, nil
	}

	// check all gpu status
	var slavePodNames []string
	for _, removeGPU := range removeGPUs {
		slavePodNames = append(slavePodNames, removeGPU.PodName)
		gpuProc, err := util.GetPodGPUProcesses(targetPod, removeGPU)
		if err != nil {
			Logger.Error("Failed to get process info on GPU: ", removeGPU.DeviceFilePath)
			Logger.Error(err)
			return nil, err
		}
		if gpuProc != nil && !request.Force {
			Logger.Info("GPU: ", removeGPU.DeviceFilePath, " status in Pod: ", targetPod.Name, " in Namespace: ", targetPod.Namespace, " is busy")
			return &gpu_mount.RemoveGPUResponse{
				RemoveGpuResult: gpu_mount.RemoveGPUResponse_GPUBusy,
			}, nil
		}
	}

	for _, removeGPU := range removeGPUs {
		err := util.UnmountGPU(targetPod, removeGPU, request.Force)
		if err != nil {
			if err.Error() == string(gpu_mount.RemoveGPUResponse_GPUBusy) {
				return &gpu_mount.RemoveGPUResponse{
					RemoveGpuResult: gpu_mount.RemoveGPUResponse_GPUBusy,
				}, nil
			}
			Logger.Error("Failed unmount GPU: ", removeGPU.DeviceFilePath, " on Pod: ", removeGPU.PodName, " in Namespace: ", removeGPU.Namespace)
			Logger.Error(err)
			return nil, err
		}
		Logger.Info("Successfully unmount GPU: ", removeGPU.DeviceFilePath)
	}

	// delete slave pod
	err = gpuMountImpl.DeleteSlavePods(slavePodNames)
	if err != nil {
		Logger.Error(err)
		return nil, err
	}
	return &gpu_mount.RemoveGPUResponse{
		RemoveGpuResult: gpu_mount.RemoveGPUResponse_Success,
	}, nil
}
