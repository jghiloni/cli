package v3

import (
	"os"

	"code.cloudfoundry.org/cli/actor/pushaction"
	"code.cloudfoundry.org/cli/actor/sharedaction"
	"code.cloudfoundry.org/cli/actor/v2action"
	"code.cloudfoundry.org/cli/actor/v3action"
	"code.cloudfoundry.org/cli/command"
	"code.cloudfoundry.org/cli/command/flag"
	sharedV2 "code.cloudfoundry.org/cli/command/v2/shared"
	"code.cloudfoundry.org/cli/command/v3/shared"
)

//go:generate counterfeiter . V2PushActor

type V2PushActor interface {
	CreateAndBindApplicationRoutes(orgGUID string, spaceGUID string, app v2action.Application) (pushaction.Warnings, error)
}

//go:generate counterfeiter . V3PushActor

type V3PushActor interface {
	CreateApplicationByNameAndSpace(name string, spaceGUID string) (v3action.Application, v3action.Warnings, error)
	CreateAndUploadPackageByApplicationNameAndSpace(appName string, spaceGUID string, bitsPath string) (v3action.Package, v3action.Warnings, error)
	StagePackage(packageGUID string) (<-chan v3action.Build, <-chan v3action.Warnings, <-chan error)
	GetStreamingLogsForApplicationByNameAndSpace(appName string, spaceGUID string, client v3action.NOAAClient) (<-chan *v3action.LogMessage, <-chan error, v3action.Warnings, error)
	SetApplicationDroplet(appName string, spaceGUID string, dropletGUID string) (v3action.Warnings, error)
	StartApplication(appName string, spaceGUID string) (v3action.Application, v3action.Warnings, error)
	StopApplication(appName string, spaceGUID string) (v3action.Warnings, error)
	GetApplicationSummaryByNameAndSpace(appName string, spaceGUID string) (v3action.ApplicationSummary, v3action.Warnings, error)
	GetApplicationByNameAndSpace(appName string, spaceGUID string) (v3action.Application, v3action.Warnings, error)
	PollStart(appGUID string, warnings chan<- v3action.Warnings) error
}

type V3PushCommand struct {
	RequiredArgs flag.AppName `positional-args:"yes"`
	usage        interface{}  `usage:"cf v3-push APP_NAME"`
	NoRoute      bool         `long:"no-route" description:"Do not map a route to this app"`

	UI                  command.UI
	Config              command.Config
	NOAAClient          v3action.NOAAClient
	SharedActor         command.SharedActor
	Actor               V3PushActor
	V2PushActor         V2PushActor
	AppSummaryDisplayer shared.AppSummaryDisplayer
}

func (cmd *V3PushCommand) Setup(config command.Config, ui command.UI) error {
	cmd.UI = ui
	cmd.Config = config
	cmd.SharedActor = sharedaction.NewActor()

	ccClient, uaaClient, err := shared.NewClients(config, ui, true)
	if err != nil {
		return err
	}
	cmd.Actor = v3action.NewActor(ccClient, config)

	ccClientV2, uaaClientV2, err := sharedV2.NewClients(config, ui, true)
	if err != nil {
		return err
	}

	v2Actor := v2action.NewActor(ccClientV2, uaaClientV2)
	cmd.V2PushActor = pushaction.NewActor(v2Actor)
	v2AppActor := v2action.NewActor(ccClientV2, uaaClientV2)

	dopplerURL, err := hackDopplerURLFromUAA(ccClient.UAA())
	if err != nil {
		return err
	}
	cmd.NOAAClient = shared.NewNOAAClient(dopplerURL, config, uaaClient, ui)

	cmd.AppSummaryDisplayer = shared.AppSummaryDisplayer{
		UI:              cmd.UI,
		Config:          cmd.Config,
		Actor:           cmd.Actor,
		V2AppRouteActor: v2AppActor,
		AppName:         cmd.RequiredArgs.AppName,
	}
	return nil
}

func (cmd V3PushCommand) Execute(args []string) error {
	cmd.UI.DisplayText(command.ExperimentalWarning)
	cmd.UI.DisplayNewline()

	err := cmd.SharedActor.CheckTarget(cmd.Config, true, true)
	if err != nil {
		return shared.HandleError(err)
	}

	user, err := cmd.Config.CurrentUser()
	if err != nil {
		return err
	}

	app, appExists, err := cmd.createApplication(user.Name)
	if err != nil {
		return shared.HandleError(err)
	}

	if appExists {
		app, err = cmd.updateApplication(user.Name)
		if err != nil {
			return shared.HandleError(err)
		}
	}

	pkg, err := cmd.uploadPackage(user.Name)
	if err != nil {
		return shared.HandleError(err)
	}

	dropletGUID, err := cmd.stagePackage(pkg, user.Name)
	if err != nil {
		return shared.HandleError(err)
	}

	if appExists {
		err = cmd.stopApplication(user.Name)
		if err != nil {
			return shared.HandleError(err)
		}
	}

	err = cmd.setApplicationDroplet(dropletGUID, user.Name)
	if err != nil {
		return shared.HandleError(err)
	}

	if !cmd.NoRoute {
		err = cmd.createAndBindRoutes(app)
		if err != nil {
			return shared.HandleError(err)
		}
	}

	err = cmd.startApplication(user.Name)
	if err != nil {
		return shared.HandleError(err)
	}

	cmd.UI.DisplayText("Waiting for app to start...")

	warnings := make(chan v3action.Warnings)
	done := make(chan bool)
	go func() {
		for {
			select {
			case message := <-warnings:
				cmd.UI.DisplayWarnings(message)
			case <-done:
				return
			}
		}
	}()

	err = cmd.Actor.PollStart(app.GUID, warnings)
	done <- true

	if err != nil {
		if _, ok := err.(v3action.StartupTimeoutError); ok {
			return shared.StartupTimeoutError{AppName: cmd.RequiredArgs.AppName}
		} else {
			return shared.HandleError(err)
		}
	}

	return cmd.AppSummaryDisplayer.DisplayAppInfo()
}

func (cmd V3PushCommand) createApplication(userName string) (v3action.Application, bool, error) {
	app, warnings, err := cmd.Actor.CreateApplicationByNameAndSpace(cmd.RequiredArgs.AppName, cmd.Config.TargetedSpace().GUID)
	cmd.UI.DisplayWarnings(warnings)

	if _, ok := err.(v3action.ApplicationAlreadyExistsError); ok {
		return app, true, nil
	} else if err != nil {
		return v3action.Application{}, false, err
	}

	cmd.UI.DisplayTextWithFlavor("Creating app {{.AppName}} in org {{.CurrentOrg}} / space {{.CurrentSpace}} as {{.CurrentUser}}...", map[string]interface{}{
		"AppName":      cmd.RequiredArgs.AppName,
		"CurrentSpace": cmd.Config.TargetedSpace().Name,
		"CurrentOrg":   cmd.Config.TargetedOrganization().Name,
		"CurrentUser":  userName,
	})

	cmd.UI.DisplayOK()
	cmd.UI.DisplayNewline()
	return app, false, nil
}

func (cmd V3PushCommand) updateApplication(userName string) (v3action.Application, error) {
	cmd.UI.DisplayTextWithFlavor("Updating app {{.AppName}} in org {{.CurrentOrg}} / space {{.CurrentSpace}} as {{.CurrentUser}}...", map[string]interface{}{
		"AppName":      cmd.RequiredArgs.AppName,
		"CurrentSpace": cmd.Config.TargetedSpace().Name,
		"CurrentOrg":   cmd.Config.TargetedOrganization().Name,
		"CurrentUser":  userName,
	})

	app, warnings, err := cmd.Actor.GetApplicationByNameAndSpace(cmd.RequiredArgs.AppName, cmd.Config.TargetedSpace().GUID)
	cmd.UI.DisplayWarnings(warnings)
	if err != nil {
		return v3action.Application{}, err
	}

	cmd.UI.DisplayOK()
	cmd.UI.DisplayNewline()
	return app, nil
}

func (cmd V3PushCommand) createAndBindRoutes(app v3action.Application) error {
	cmd.UI.DisplayText("Mapping routes...")
	routeWarnings, err := cmd.V2PushActor.CreateAndBindApplicationRoutes(cmd.Config.TargetedOrganization().GUID, cmd.Config.TargetedSpace().GUID, v2action.Application{Name: app.Name, GUID: app.GUID})
	cmd.UI.DisplayWarnings(routeWarnings)
	if err != nil {
		return err
	}

	cmd.UI.DisplayOK()
	cmd.UI.DisplayNewline()
	return nil
}

func (cmd V3PushCommand) uploadPackage(userName string) (v3action.Package, error) {
	cmd.UI.DisplayTextWithFlavor("Uploading app {{.AppName}} in org {{.CurrentOrg}} / space {{.CurrentSpace}} as {{.CurrentUser}}...", map[string]interface{}{
		"AppName":      cmd.RequiredArgs.AppName,
		"CurrentSpace": cmd.Config.TargetedSpace().Name,
		"CurrentOrg":   cmd.Config.TargetedOrganization().Name,
		"CurrentUser":  userName,
	})

	pwd, err := os.Getwd()
	if err != nil {
		return v3action.Package{}, err
	}

	pkg, warnings, err := cmd.Actor.CreateAndUploadPackageByApplicationNameAndSpace(cmd.RequiredArgs.AppName, cmd.Config.TargetedSpace().GUID, pwd)
	cmd.UI.DisplayWarnings(warnings)
	if err != nil {
		return v3action.Package{}, err
	}

	cmd.UI.DisplayOK()
	cmd.UI.DisplayNewline()
	return pkg, nil
}

func (cmd V3PushCommand) stagePackage(pkg v3action.Package, userName string) (string, error) {
	cmd.UI.DisplayTextWithFlavor("Staging package for app {{.AppName}} in org {{.OrgName}} / space {{.SpaceName}} as {{.Username}}...", map[string]interface{}{
		"AppName":   cmd.RequiredArgs.AppName,
		"OrgName":   cmd.Config.TargetedOrganization().Name,
		"SpaceName": cmd.Config.TargetedSpace().Name,
		"Username":  userName,
	})

	logStream, logErrStream, logWarnings, logErr := cmd.Actor.GetStreamingLogsForApplicationByNameAndSpace(cmd.RequiredArgs.AppName, cmd.Config.TargetedSpace().GUID, cmd.NOAAClient)
	cmd.UI.DisplayWarnings(logWarnings)
	if logErr != nil {
		return "", logErr
	}

	buildStream, warningsStream, errStream := cmd.Actor.StagePackage(pkg.GUID)
	err, dropletGUID := shared.PollStage(buildStream, warningsStream, errStream, logStream, logErrStream, cmd.UI)
	if err != nil {
		return "", err
	}

	cmd.UI.DisplayOK()
	cmd.UI.DisplayNewline()
	return dropletGUID, nil
}

func (cmd V3PushCommand) setApplicationDroplet(dropletGUID string, userName string) error {
	cmd.UI.DisplayTextWithFlavor("Setting app {{.AppName}} to droplet {{.DropletGUID}} in org {{.OrgName}} / space {{.SpaceName}} as {{.Username}}...", map[string]interface{}{
		"AppName":     cmd.RequiredArgs.AppName,
		"DropletGUID": dropletGUID,
		"OrgName":     cmd.Config.TargetedOrganization().Name,
		"SpaceName":   cmd.Config.TargetedSpace().Name,
		"Username":    userName,
	})

	warnings, err := cmd.Actor.SetApplicationDroplet(cmd.RequiredArgs.AppName, cmd.Config.TargetedSpace().GUID, dropletGUID)
	cmd.UI.DisplayWarnings(warnings)
	if err != nil {
		return err
	}

	cmd.UI.DisplayOK()
	cmd.UI.DisplayNewline()
	return nil
}

func (cmd V3PushCommand) startApplication(userName string) error {
	cmd.UI.DisplayTextWithFlavor("Starting app {{.AppName}} in org {{.OrgName}} / space {{.SpaceName}} as {{.Username}}...", map[string]interface{}{
		"AppName":   cmd.RequiredArgs.AppName,
		"OrgName":   cmd.Config.TargetedOrganization().Name,
		"SpaceName": cmd.Config.TargetedSpace().Name,
		"Username":  userName,
	})

	_, warnings, err := cmd.Actor.StartApplication(cmd.RequiredArgs.AppName, cmd.Config.TargetedSpace().GUID)
	cmd.UI.DisplayWarnings(warnings)
	if err != nil {
		return err
	}
	cmd.UI.DisplayOK()
	cmd.UI.DisplayNewline()
	return nil
}

func (cmd V3PushCommand) stopApplication(userName string) error {
	cmd.UI.DisplayTextWithFlavor("Stopping app {{.AppName}} in org {{.CurrentOrg}} / space {{.CurrentSpace}} as {{.CurrentUser}}...", map[string]interface{}{
		"AppName":      cmd.RequiredArgs.AppName,
		"CurrentSpace": cmd.Config.TargetedSpace().Name,
		"CurrentOrg":   cmd.Config.TargetedOrganization().Name,
		"CurrentUser":  userName,
	})

	warnings, err := cmd.Actor.StopApplication(cmd.RequiredArgs.AppName, cmd.Config.TargetedSpace().GUID)
	cmd.UI.DisplayWarnings(warnings)
	if err != nil {
		return err
	}
	cmd.UI.DisplayOK()
	cmd.UI.DisplayNewline()
	return nil
}
