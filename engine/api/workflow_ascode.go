package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/ovh/cds/engine/api/application"
	"github.com/ovh/cds/engine/api/ascode"
	"github.com/ovh/cds/engine/api/operation"
	"github.com/ovh/cds/engine/api/project"
	"github.com/ovh/cds/engine/api/workflow"
	"github.com/ovh/cds/engine/service"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/exportentities"
	v2 "github.com/ovh/cds/sdk/exportentities/v2"
)

// postWorkflowAsCodeHandler update an ascode workflow, this will create a pull request to target repository.
func (api *API) postWorkflowAsCodeHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		workflowName := vars["permWorkflowName"]

		migrate := FormBool(r, "migrate")
		branch := FormString(r, "branch")
		message := FormString(r, "message")

		if branch == "" || message == "" {
			return sdk.NewErrorFrom(sdk.ErrWrongRequest, "missing branch or message data")
		}

		u := getAPIConsumer(ctx)
		p, err := project.Load(ctx, api.mustDB(), key,
			project.LoadOptions.WithApplicationWithDeploymentStrategies,
			project.LoadOptions.WithPipelines,
			project.LoadOptions.WithEnvironments,
			project.LoadOptions.WithIntegrations,
			project.LoadOptions.WithClearKeys,
		)
		if err != nil {
			return err
		}

		projIdent := sdk.ProjectIdentifiers{ID: p.ID, Key: p.Key}
		wfDB, err := workflow.Load(ctx, api.mustDB(), projIdent, workflowName, workflow.LoadOptions{
			DeepPipeline:          migrate,
			WithAsCodeUpdateEvent: migrate,
			WithTemplate:          true,
		})
		if err != nil {
			return err
		}

		var rootApp *sdk.Application
		if wfDB.WorkflowData.Node.Context != nil && wfDB.WorkflowData.Node.Context.ApplicationID != 0 {
			rootApp, err = application.LoadByIDWithClearVCSStrategyPassword(api.mustDB(), wfDB.WorkflowData.Node.Context.ApplicationID)
			if err != nil {
				return err
			}
		}
		if rootApp == nil {
			return sdk.NewErrorFrom(sdk.ErrWrongRequest, "cannot find the root application of the workflow")
		}

		if migrate {
			if rootApp.VCSServer == "" || rootApp.RepositoryFullname == "" {
				return sdk.NewErrorFrom(sdk.ErrRepoNotFound, "no vcs configuration set on the root application of the given workflow")
			}
			return api.migrateWorkflowAsCode(ctx, w, *p, wfDB, *rootApp, branch, message)
		}

		if wfDB.FromRepository == "" {
			return sdk.NewErrorFrom(sdk.ErrForbidden, "cannot update a workflow that is not ascode")
		}

		if wfDB.TemplateInstance != nil {
			return sdk.NewErrorFrom(sdk.ErrForbidden, "cannot update a workflow that was generated by a template")
		}

		var wf sdk.Workflow
		if err := service.UnmarshalBody(r, &wf); err != nil {
			return err
		}

		if err := workflow.RenameNode(ctx, api.mustDB(), &wf); err != nil {
			return err
		}
		if err := workflow.CheckValidity(ctx, api.mustDB(), &wf); err != nil {
			return err
		}
		if err := workflow.CompleteWorkflow(ctx, api.mustDB(), &wf, projIdent, workflow.LoadOptions{DeepPipeline: true}); err != nil {
			return err
		}

		var data exportentities.WorkflowComponents
		data.Workflow, err = exportentities.NewWorkflow(ctx, wf, v2.WorkflowSkipIfOnlyOneRepoWebhook)
		if err != nil {
			return err
		}

		tx, err := api.mustDB().Begin()
		if err != nil {
			return sdk.WithStack(err)
		}
		defer tx.Rollback() // nolint

		ope, err := operation.PushOperationUpdate(ctx, tx, api.Cache, *p, data, rootApp.VCSServer, rootApp.RepositoryFullname, branch, message, rootApp.RepositoryStrategy, u)
		if err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return sdk.WithStack(err)
		}

		sdk.GoRoutine(context.Background(), fmt.Sprintf("UpdateAsCodeResult-%s", ope.UUID), func(ctx context.Context) {
			ed := ascode.EntityData{
				Name:          wfDB.Name,
				ID:            wfDB.ID,
				Type:          ascode.WorkflowEvent,
				FromRepo:      wfDB.FromRepository,
				OperationUUID: ope.UUID,
			}
			ascode.UpdateAsCodeResult(ctx, api.mustDB(), api.Cache, projIdent, *wfDB, *rootApp, ed, u)
		}, api.PanicDump())

		return service.WriteJSON(w, sdk.Operation{
			UUID:   ope.UUID,
			Status: ope.Status,
		}, http.StatusOK)
	}
}

func (api *API) migrateWorkflowAsCode(ctx context.Context, w http.ResponseWriter, p sdk.Project, wf *sdk.Workflow, app sdk.Application, branch, message string) error {
	u := getAPIConsumer(ctx)

	projIdent := sdk.ProjectIdentifiers{ID: p.ID, Key: p.Key}

	if wf.FromRepository != "" || (wf.FromRepository == "" && len(wf.AsCodeEvent) > 0) {
		return sdk.WithStack(sdk.ErrWorkflowAlreadyAsCode)
	}

	tx, err := api.mustDB().Begin()
	if err != nil {
		return sdk.WithStack(err)
	}
	defer tx.Rollback() // nolint

	// Check if there is a repository web hook
	found := false
	for _, h := range wf.WorkflowData.GetHooks() {
		if h.HookModelName == sdk.RepositoryWebHookModelName {
			found = true
			break
		}
	}
	if !found {
		h := sdk.NodeHook{
			Config:        sdk.RepositoryWebHookModel.DefaultConfig.Clone(),
			HookModelName: sdk.RepositoryWebHookModel.Name,
		}
		wf.WorkflowData.Node.Hooks = append(wf.WorkflowData.Node.Hooks, h)

		if err := workflow.Update(ctx, tx, api.Cache, projIdent, wf, workflow.UpdateOptions{}); err != nil {
			return err
		}
	}

	data, err := workflow.Pull(ctx, tx, projIdent, wf.Name, project.EncryptWithBuiltinKey, v2.WorkflowSkipIfOnlyOneRepoWebhook)
	if err != nil {
		return err
	}

	ope, err := operation.PushOperation(ctx, tx, api.Cache, p, data, app.VCSServer, app.RepositoryFullname, branch, message, app.RepositoryStrategy, u)
	if err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return sdk.WithStack(err)
	}

	sdk.GoRoutine(context.Background(), fmt.Sprintf("MigrateWorkflowAsCodeResult-%s", ope.UUID), func(ctx context.Context) {
		ed := ascode.EntityData{
			FromRepo:      ope.URL,
			Type:          ascode.WorkflowEvent,
			ID:            wf.ID,
			Name:          wf.Name,
			OperationUUID: ope.UUID,
		}
		ascode.UpdateAsCodeResult(ctx, api.mustDB(), api.Cache, projIdent, *wf, app, ed, u)
	}, api.PanicDump())

	return service.WriteJSON(w, sdk.Operation{
		UUID:   ope.UUID,
		Status: ope.Status,
	}, http.StatusOK)
}
