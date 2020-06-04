package application_test

import (
	"context"
	"testing"

	"github.com/ovh/cds/sdk"

	"github.com/ovh/cds/engine/api/application"
	"github.com/ovh/cds/engine/api/test"
	"github.com/ovh/cds/engine/api/test/assets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_DAOVariable(t *testing.T) {
	db, cache := test.SetupPG(t)

	key := sdk.RandomString(10)
	proj := assets.InsertTestProject(t, db, cache, key, key)
	app := sdk.Application{
		Name: "my-app",
	}
	require.NoError(t, application.Insert(db, proj.ID, &app))

	u, _ := assets.InsertLambdaUser(t, db, &proj.ProjectGroups[0].Group)

	v1 := &sdk.Variable{Name: "clear", Type: sdk.TextVariable, Value: "clear_value"}
	v2 := &sdk.Variable{Name: "secret", Type: sdk.SecretVariable, Value: "secret_value"}

	require.NoError(t, application.InsertVariable(db, app.ID, v1, u))
	assert.Equal(t, "clear_value", v1.Value)

	require.NoError(t, application.InsertVariable(db, app.ID, v2, u))
	assert.Equal(t, sdk.PasswordPlaceholder, v2.Value)

	vs, err := application.LoadVariables(context.TODO(), db, app.ID)
	require.NoError(t, err)
	assert.Equal(t, "clear_value", vs[0].Value)
	assert.Equal(t, sdk.PasswordPlaceholder, vs[1].Value)

	vs, err = application.LoadVariablesWithDecrytion(context.TODO(), db, app.ID)
	require.NoError(t, err)
	assert.Equal(t, "clear_value", vs[0].Value)
	assert.Equal(t, "secret_value", vs[1].Value)

	require.NoError(t, application.UpdateVariable(db, app.ID, &vs[1], &vs[1], u))

	v1, err = application.LoadVariable(context.TODO(), db, app.ID, "clear")
	require.NoError(t, err)
	assert.Equal(t, "clear_value", v1.Value)

	v2, err = application.LoadVariable(context.TODO(), db, app.ID, "secret")
	require.NoError(t, err)
	assert.Equal(t, sdk.PasswordPlaceholder, v2.Value)

	v2, err = application.LoadVariableWithDecryption(context.TODO(), db, app.ID, v2.ID, "secret")
	require.NoError(t, err)
	assert.Equal(t, "secret_value", v2.Value)

	require.NoError(t, application.DeleteVariable(db, app.ID, v2, u))

	require.NoError(t, application.DeleteVariablesByApplicationID(db, app.ID))

}