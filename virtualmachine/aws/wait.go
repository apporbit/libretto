package aws

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
)

const (
	// Timeout for VM operations viz. Halt, Start & Terminate in seconds
	VmOpsTimeout = 900 // 15 mins

	// Retry interval for VM operations in seconds
	VmOpsInterval = 15

	// Timeout for Volume operations viz. Create, Detach in seconds
	VolTimeout = 600 // 10 mins

	// Retry interval for Volume operations in seconds
	VolInterval = 5
)

// ReadyError is an information error that tells you why an instance wasn't
// ready.
type ReadyError struct {
	Err error

	ImageID               string
	InstanceID            string
	InstanceType          string
	LaunchTime            time.Time
	PublicIPAddress       string
	State                 string
	StateReason           string
	StateTransitionReason string
	SubnetID              string
	VPCID                 string
}

// Error returns a summarized string version of ReadyError. More details about
// the failed instance can be accessed through the struct.
func (e ReadyError) Error() string {
	return fmt.Sprintf(
		"failed waiting for instance (%s) to be ready, reason was: %s",
		e.InstanceID,
		e.StateReason,
	)
}

func newReadyError(out *ec2.DescribeInstancesOutput) ReadyError {
	if len(out.Reservations) < 1 {
		return ReadyError{Err: ErrNoInstance}
	}
	if len(out.Reservations[0].Instances) < 1 {
		return ReadyError{Err: ErrNoInstance}
	}

	var rerr ReadyError

	if v := out.Reservations[0].Instances[0].ImageId; v != nil {
		rerr.ImageID = *v
	}
	if v := out.Reservations[0].Instances[0].InstanceId; v != nil {
		rerr.InstanceID = *v
	}
	if v := out.Reservations[0].Instances[0].InstanceType; v != nil {
		rerr.InstanceType = *v
	}
	if v := out.Reservations[0].Instances[0].LaunchTime; v != nil {
		rerr.LaunchTime = *v
	}
	if v := out.Reservations[0].Instances[0].PublicIpAddress; v != nil {
		rerr.PublicIPAddress = *v
	}
	if v := out.Reservations[0].Instances[0].State; v != nil {
		if v.Name != nil {
			rerr.State = *v.Name
		}
	}
	if v := out.Reservations[0].Instances[0].StateReason; v != nil {
		if v.Message != nil {
			rerr.StateReason = *v.Message
		}
	}
	if v := out.Reservations[0].Instances[0].StateTransitionReason; v != nil {
		rerr.StateTransitionReason = *v
	}
	if v := out.Reservations[0].Instances[0].SubnetId; v != nil {
		rerr.SubnetID = *v
	}
	if v := out.Reservations[0].Instances[0].VpcId; v != nil {
		rerr.VPCID = *v
	}

	return rerr
}

func waitUntilReady(svc *ec2.EC2, instanceID string) error {
	// With 10 retries, total timeout is about 17 minutes.
	const maxRetries = 10

	var resp *ec2.DescribeInstancesOutput
	var err error

	for ii := 0; ii < maxRetries; ii++ {
		// Sleep will be 1, 2, 4, 8, 16...
		// time.Sleep(2ⁱ * time.Second)
		time.Sleep(time.Duration(math.Exp2(float64(ii))) * time.Second)

		resp, err = svc.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{&instanceID},
		})
		if err != nil {
			continue
		}

		if len(resp.Reservations) < 1 {
			continue
		}
		if len(resp.Reservations[0].Instances) < 1 {
			continue
		}
		if resp.Reservations[0].Instances[0].State == nil {
			continue
		}
		if resp.Reservations[0].Instances[0].State.Name == nil {
			continue
		}

		state := *resp.Reservations[0].Instances[0].State.Name
		switch state {
		case ec2.InstanceStateNameRunning:
			// We're ready!
			return nil
		case ec2.InstanceStateNameTerminated, ec2.InstanceStateNameShuttingDown:
			// Polling is useless. This instance isn't coming up. Return error.
			return fmt.Errorf("Instance unexpectedly terminating " +
				"or has terminated")
		case ec2.InstanceStateNameStopping, ec2.InstanceStateNameStopped:
			// Polling is useless. This instance isn't coming up. Return error.
			return fmt.Errorf("Instance unexpectedly stopping " +
				"or has stopped")
		}
	}

	rerr := newReadyError(resp)

	if err != nil {
		rerr.Err = err
	} else {
		rerr.Err = errors.New("wait until instance ready timeout")
	}

	return rerr
}

// waitUntilStopped: waits until vm is stopped
func waitUntilStopped(svc *ec2.EC2, instanceID string) error {
	matchBreakState := make(map[string]bool)

	matchBreakState[ec2.InstanceStateNameStopped] = true
	matchBreakState[ec2.InstanceStateNameTerminated] = false
	matchBreakState[ec2.InstanceStateNameShuttingDown] = false

	return waitUntilState(svc, instanceID, matchBreakState)
}

// waitUntilRunning: waits until vm is running
func waitUntilRunning(svc *ec2.EC2, instanceID string) error {
	matchBreakState := make(map[string]bool)

	matchBreakState[ec2.InstanceStateNameRunning] = true
	matchBreakState[ec2.InstanceStateNameTerminated] = false
	matchBreakState[ec2.InstanceStateNameShuttingDown] = false

	return waitUntilState(svc, instanceID, matchBreakState)
}

// waitUntilTerminated: waits until vm is terminated
func waitUntilTerminated(svc *ec2.EC2, instanceID string) error {
	matchBreakState := make(map[string]bool)

	matchBreakState[ec2.InstanceStateNameTerminated] = true

	return waitUntilState(svc, instanceID, matchBreakState)
}

// waitUntilState: waits until vm is in given state
func waitUntilState(svc *ec2.EC2, instanceID string,
	matchBreakState map[string]bool) error {
	var instanceStatus *InstanceStatus
	var err error

	const maxRetries = VmOpsTimeout / VmOpsInterval
	for ii := 0; ii < maxRetries; ii++ {
		// Sleep will be VmOpsInterval, 2*VmOpsInterval, 3*VmOpsInterval, ...
		time.Sleep(time.Duration(VmOpsInterval) * time.Second)

		instanceStatus, err = GetInstanceStatus(svc, instanceID)
		if err != nil {
			continue
		}
		state := instanceStatus.State

		if val, ok := matchBreakState[state]; ok {
			if val {
				// true val means its state to be matched
				return nil
			}
			// false val means an unexpected state has reached
			// Polling is useless. So return appropriate error
			return fmt.Errorf("Instance unexpectedly %s", state)
		}
	}

	for state, val := range matchBreakState {
		if val {
			return fmt.Errorf("Timeout while waiting for VM to be %s", state)
		}
	}
	return fmt.Errorf("No match state given")
}

// waitForCreate: waits for volume to get created
func waitForCreate(svc *ec2.EC2, volumeID string) error {
	return waitUntilVolumeState(svc, volumeID, ec2.VolumeStateAvailable)
}

// waitForAttach: waits for volume to attach to a instance
func waitForAttach(svc *ec2.EC2, volumeID string) error {
	return waitUntilVolumeState(svc, volumeID, ec2.VolumeStateInUse)
}

// waitForDetach: waits for volume to detach from a instance
func waitForDetach(svc *ec2.EC2, volumeID string) error {
	return waitUntilVolumeState(svc, volumeID, ec2.VolumeStateAvailable)
}

// waitUntiVolumeState: waits for volume to get to state given as argument
func waitUntilVolumeState(svc *ec2.EC2, volumeID string, stateEnum string) error {
	var resp *ec2.DescribeVolumesOutput
	var err error

	const maxRetries = VolTimeout / VolInterval
	for ii := 0; ii < maxRetries; ii++ {
		// Sleep will be VolInterval, 2*VolInterval, 3*VolInterval, ...
		time.Sleep(time.Duration(VolInterval) * time.Second)

		resp, err = svc.DescribeVolumes(&ec2.DescribeVolumesInput{
			VolumeIds: []*string{&volumeID},
		})
		if err != nil {
			continue
		}

		if len(resp.Volumes) < 1 {
			continue
		}
		if resp.Volumes[0].State == nil {
			continue
		}
		state := *resp.Volumes[0].State
		if state == ec2.VolumeStateError {
			return fmt.Errorf("Volume in error state")
		}
		if state == stateEnum {
			// volume reached required state
			return nil
		}
	}

	if err != nil {
		return fmt.Errorf("Error in getting volume state: %v", err)
	}

	return fmt.Errorf("Timeout while waiting for Volume to be %s", stateEnum)
}
