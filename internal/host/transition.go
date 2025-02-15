package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/filanov/bm-inventory/internal/events"

	"github.com/filanov/bm-inventory/internal/common"
	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/swag"

	"github.com/filanov/bm-inventory/models"
	logutil "github.com/filanov/bm-inventory/pkg/log"
	"github.com/filanov/stateswitch"
	"github.com/jinzhu/gorm"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/thoas/go-funk"
)

type transitionHandler struct {
	db  *gorm.DB
	log logrus.FieldLogger
}

////////////////////////////////////////////////////////////////////////////
// RegisterHost
////////////////////////////////////////////////////////////////////////////

type TransitionArgsRegisterHost struct {
	ctx                   context.Context
	discoveryAgentVersion string
}

func (th *transitionHandler) PostRegisterHost(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostRegisterHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsRegisterHost)
	if !ok {
		return errors.New("PostRegisterHost invalid argument")
	}

	host := models.Host{}
	log := logutil.FromContext(params.ctx, th.log)

	// If host already exists
	if err := th.db.First(&host, "id = ? and cluster_id = ?", sHost.host.ID, sHost.host.ClusterID).Error; err == nil {
		// The reason for the double register is unknown (HW might have changed) -
		// so we reset the hw info and progress, and start the discovery process again.
		if host, err := updateHostProgress(log, th.db, sHost.host.ClusterID, *sHost.host.ID, sHost.srcState,
			swag.StringValue(sHost.host.Status), statusInfoDiscovering, sHost.host.Progress.CurrentStage, "", "",
			"inventory", "", "discovery_agent_version", params.discoveryAgentVersion, "bootstrap", false); err != nil {
			return err
		} else {
			sHost.host = host
			return nil
		}
	}

	sHost.host.StatusUpdatedAt = strfmt.DateTime(time.Now())
	sHost.host.StatusInfo = swag.String(statusInfoDiscovering)
	log.Infof("Register new host %s cluster %s", sHost.host.ID.String(), sHost.host.ClusterID)
	return th.db.Create(sHost.host).Error
}

func (th *transitionHandler) PostRegisterDuringInstallation(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("RegisterNewHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsRegisterHost)
	if !ok {
		return errors.New("PostRegisterDuringInstallation invalid argument")
	}

	return th.updateTransitionHost(logutil.FromContext(params.ctx, th.log), th.db, sHost,
		"The host unexpectedly restarted during the installation.")
}

func (th *transitionHandler) IsHostInReboot(sw stateswitch.StateSwitch, _ stateswitch.TransitionArgs) (bool, error) {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return false, errors.New("IsInReboot incompatible type of StateSwitch")
	}

	return sHost.host.Progress.CurrentStage == models.HostStageRebooting, nil
}

func (th *transitionHandler) PostRegisterDuringReboot(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("RegisterNewHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsRegisterHost)
	if !ok {
		return errors.New("PostRegisterDuringReboot invalid argument")
	}

	return th.updateTransitionHost(logutil.FromContext(params.ctx, th.log), th.db, sHost,
		"Expected the host to boot from disk, but it booted the installation image. Please reboot and fix boot order to boot from disk.")
}

////////////////////////////////////////////////////////////////////////////
// Installation failure
////////////////////////////////////////////////////////////////////////////

type TransitionArgsHostInstallationFailed struct {
	ctx    context.Context
	reason string
}

func (th *transitionHandler) PostHostInstallationFailed(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("HostInstallationFailed incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsHostInstallationFailed)
	if !ok {
		return errors.New("HostInstallationFailed invalid argument")
	}

	return th.updateTransitionHost(logutil.FromContext(params.ctx, th.log), th.db, sHost,
		params.reason)
}

////////////////////////////////////////////////////////////////////////////
// Cancel Installation
////////////////////////////////////////////////////////////////////////////

type TransitionArgsCancelInstallation struct {
	ctx    context.Context
	reason string
	db     *gorm.DB
}

func (th *transitionHandler) PostCancelInstallation(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostCancelInstallation incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsCancelInstallation)
	if !ok {
		return errors.New("PostCancelInstallation invalid argument")
	}
	if sHost.srcState == HostStatusError {
		return nil
	}

	return th.updateTransitionHost(logutil.FromContext(params.ctx, th.log), params.db, sHost,
		params.reason)
}

////////////////////////////////////////////////////////////////////////////
// Reset Host
////////////////////////////////////////////////////////////////////////////

type TransitionArgsResetHost struct {
	ctx    context.Context
	reason string
	db     *gorm.DB
}

func (th *transitionHandler) PostResetHost(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostResetHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsResetHost)
	if !ok {
		return errors.New("PostResetHost invalid argument")
	}

	return th.updateTransitionHost(logutil.FromContext(params.ctx, th.log), params.db, sHost,
		params.reason)
}

////////////////////////////////////////////////////////////////////////////
// Install host
////////////////////////////////////////////////////////////////////////////

type TransitionArgsInstallHost struct {
	ctx context.Context
	db  *gorm.DB
}

func (th *transitionHandler) PostInstallHost(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostInstallHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsInstallHost)
	if !ok {
		return errors.New("PostInstallHost invalid argument")
	}
	return th.updateTransitionHost(logutil.FromContext(params.ctx, th.log), params.db, sHost,
		statusInfoInstalling)
}

////////////////////////////////////////////////////////////////////////////
// Disable host
////////////////////////////////////////////////////////////////////////////

type TransitionArgsDisableHost struct {
	ctx context.Context
}

func (th *transitionHandler) PostDisableHost(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostDisableHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsDisableHost)
	if !ok {
		return errors.New("PostDisableHost invalid argument")
	}

	return th.updateTransitionHost(logutil.FromContext(params.ctx, th.log), th.db, sHost,
		statusInfoDisabled)
}

////////////////////////////////////////////////////////////////////////////
// Enable host
////////////////////////////////////////////////////////////////////////////

type TransitionArgsEnableHost struct {
	ctx context.Context
}

func (th *transitionHandler) PostEnableHost(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostEnableHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsEnableHost)
	if !ok {
		return errors.New("PostEnableHost invalid argument")
	}

	return th.updateTransitionHost(logutil.FromContext(params.ctx, th.log), th.db, sHost,
		statusInfoDiscovering, "inventory", "")
}

////////////////////////////////////////////////////////////////////////////
// Resetting pending user action
////////////////////////////////////////////////////////////////////////////

type TransitionResettingPendingUserAction struct {
	ctx context.Context
	db  *gorm.DB
}

func (th *transitionHandler) IsValidRoleForInstallation(sw stateswitch.StateSwitch, _ stateswitch.TransitionArgs) (bool, error) {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return false, errors.New("IsValidRoleForInstallation incompatible type of StateSwitch")
	}
	validRoles := []string{string(models.HostRoleMaster), string(models.HostRoleWorker)}
	if !funk.ContainsString(validRoles, string(sHost.host.Role)) {
		return false, common.NewApiError(http.StatusConflict,
			errors.Errorf("Can't install host %s due to invalid host role: %s, should be one of %s",
				sHost.host.ID.String(), sHost.host.Role, validRoles))
	}
	return true, nil
}

func (th *transitionHandler) PostResettingPendingUserAction(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("ResettingPendingUserAction incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionResettingPendingUserAction)
	if !ok {
		return errors.New("ResettingPendingUserAction invalid argument")
	}

	return th.updateTransitionHost(logutil.FromContext(params.ctx, th.log), params.db, sHost,
		statusInfoResettingPendingUserAction)
}

////////////////////////////////////////////////////////////////////////////
// Resetting pending user action
////////////////////////////////////////////////////////////////////////////

type TransitionArgsPrepareForInstallation struct {
	ctx context.Context
	db  *gorm.DB
}

func (th *transitionHandler) PostPrepareForInstallation(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, _ := sw.(*stateHost)
	params, _ := args.(*TransitionArgsPrepareForInstallation)
	return th.updateTransitionHost(logutil.FromContext(params.ctx, th.log), params.db, sHost,
		statusInfoPreparingForInstallation)
}

func (th *transitionHandler) updateTransitionHost(log logrus.FieldLogger, db *gorm.DB, state *stateHost,
	statusInfo string, extra ...interface{}) error {

	if host, err := updateHostStatus(log, db, state.host.ClusterID, *state.host.ID, state.srcState,
		swag.StringValue(state.host.Status), statusInfo, extra...); err != nil {
		return err
	} else {
		state.host = host
		return nil
	}
}

////////////////////////////////////////////////////////////////////////////
// Refresh Host
////////////////////////////////////////////////////////////////////////////

type TransitionArgsRefreshHost struct {
	ctx               context.Context
	eventHandler      events.Handler
	conditions        map[validationID]bool
	validationResults map[string][]validationResult
	db                *gorm.DB
}

func If(id validationID) stateswitch.Condition {
	ret := func(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) (bool, error) {
		params, ok := args.(*TransitionArgsRefreshHost)
		if !ok {
			return false, errors.Errorf("If(%s) invalid argument", id.String())
		}
		b, ok := params.conditions[id]
		if !ok {
			return false, errors.Errorf("If(%s) no such condition", id.String())
		}
		return b, nil
	}
	return ret
}

func (th *transitionHandler) IsPreparingTimedOut(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) (bool, error) {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return false, errors.New("IsPreparingTimedOut incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsRefreshHost)
	if !ok {
		return false, errors.New("IsPreparingTimedOut invalid argument")
	}
	var cluster common.Cluster
	err := params.db.Select("status").Take(&cluster, "id = ?", sHost.host.ClusterID.String()).Error
	if err != nil {
		return false, err
	}
	return swag.StringValue(cluster.Status) != models.ClusterStatusPreparingForInstallation, nil
}

func (th *transitionHandler) HasClusterError(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) (bool, error) {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return false, errors.New("HasClusterError incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsRefreshHost)
	if !ok {
		return false, errors.New("HasClusterError invalid argument")
	}
	var cluster common.Cluster
	err := params.db.Select("status").Take(&cluster, "id = ?", sHost.host.ClusterID.String()).Error
	if err != nil {
		return false, err
	}
	return swag.StringValue(cluster.Status) == models.ClusterStatusError, nil
}

// Return a post transition function with a constant reason
func (th *transitionHandler) PostRefreshHost(reason string) stateswitch.PostTransition {
	ret := func(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
		sHost, ok := sw.(*stateHost)
		if !ok {
			return errors.New("PostResetHost incompatible type of StateSwitch")
		}
		params, ok := args.(*TransitionArgsRefreshHost)
		if !ok {
			return errors.New("PostRefreshHost invalid argument")
		}
		var (
			b           []byte
			err         error
			updatedHost *models.Host
			hostname    string
		)
		b, err = json.Marshal(&params.validationResults)
		if err != nil {
			return err
		}
		updatedHost, err = updateHostStatus(logutil.FromContext(params.ctx, th.log), params.db, sHost.host.ClusterID, *sHost.host.ID,
			sHost.srcState, swag.StringValue(sHost.host.Status), reason, "validations_info", string(b))
		if err == nil && updatedHost != nil && sHost.srcState != swag.StringValue(updatedHost.Status) {
			hostname, err = common.GetCurrentHostName(updatedHost)
			if err != nil {
				hostname = updatedHost.ID.String()
			}
			msg := fmt.Sprintf("Updated status of host %s to %s", hostname, *updatedHost.Status)
			params.eventHandler.AddEvent(params.ctx, updatedHost.ID.String(), models.EventSeverityInfo, msg, time.Now(), updatedHost.ClusterID.String())
			return nil
		}
		return err
	}
	return ret
}
