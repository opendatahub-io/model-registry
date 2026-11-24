package repository_test

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/kubeflow/hub/internal/platform/db/entity"
	"github.com/kubeflow/hub/internal/platform/db/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestTypeSaveConcurrentInsert(t *testing.T) {
	for _, tc := range []struct {
		name       string
		winnerKind int32
		wantError  bool
	}{
		{name: "matching kind", winnerKind: 1},
		{name: "incompatible kind", winnerKind: 2, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sqlDB, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() {
				assert.NoError(t, sqlDB.Close())
				assert.NoError(t, mock.ExpectationsWereMet())
			})
			db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
				Logger: logger.Default.LogMode(logger.Silent),
			})
			require.NoError(t, err)

			name := "concurrent-type"
			kind := int32(1)
			input := &entity.TypeImpl{Attributes: &entity.TypeAttributes{
				Name: &name, TypeKind: &kind,
			}}
			columns := []string{"id", "name", "version", "type_kind", "description", "input_type", "output_type", "external_id"}
			mock.ExpectQuery(`SELECT .* FROM "Type"`).WithArgs(name, 1).
				WillReturnRows(sqlmock.NewRows(columns))
			mock.ExpectBegin()
			// An empty RETURNING result models an insert skipped because another
			// initializer created the type after our initial lookup.
			mock.ExpectQuery(`INSERT INTO "Type" .* ON CONFLICT \("name"\) DO NOTHING RETURNING "id"`).
				WithArgs(name, nil, kind, nil, nil, nil, nil).
				WillReturnRows(sqlmock.NewRows([]string{"id"}))
			mock.ExpectCommit()
			mock.ExpectQuery(`SELECT .* FROM "Type"`).WithArgs(name, 1).
				WillReturnRows(sqlmock.NewRows(columns).AddRow(42, name, nil, tc.winnerKind, nil, nil, nil, nil))
			mock.ExpectClose()

			saved, err := repository.NewTypeRepository(db).Save(input)
			if tc.wantError {
				require.ErrorContains(t, err, "invalid type: kind")
				assert.Same(t, input, saved)
			} else {
				require.NoError(t, err)
				require.NotNil(t, saved.GetID())
				assert.Equal(t, int32(42), *saved.GetID())
				assert.Equal(t, kind, *saved.GetAttributes().TypeKind)
			}
		})
	}
}
