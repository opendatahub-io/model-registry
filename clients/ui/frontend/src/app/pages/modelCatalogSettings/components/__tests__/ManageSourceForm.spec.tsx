import React from 'react';
import { render, screen, waitFor } from '@testing-library/react';
import { userEvent } from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import '@testing-library/jest-dom';
import ManageSourceForm from '~/app/pages/modelCatalogSettings/components/ManageSourceForm';
import {
  ModelCatalogSettingsContext,
  ModelCatalogSettingsContextType,
} from '~/app/context/modelCatalogSettings/ModelCatalogSettingsContext';
import { CatalogSourceType } from '~/app/modelCatalogTypes';
import { CatalogSourceStatus } from '~/app/shared/types/catalogTypes';

const mockNavigate = jest.fn();

jest.mock('react-router-dom', () => ({
  ...jest.requireActual('react-router-dom'),
  useNavigate: () => mockNavigate,
}));

jest.mock('~/app/pages/modelCatalogSettings/useSourcePreview', () => ({
  useSourcePreview: () => ({
    handleValidate: jest.fn(),
    isValidating: false,
    validationError: undefined,
    isValidationSuccess: false,
    clearValidationSuccess: jest.fn(),
    canPreview: false,
    handlePreview: jest.fn(),
    previewDisabledTooltip: '',
    previewState: { isLoadingInitial: false },
  }),
}));

const createMockContext = (
  overrides: Partial<ModelCatalogSettingsContextType> = {},
): ModelCatalogSettingsContextType => ({
  apiState: {
    apiAvailable: true,
    api: {
      getCatalogSourceConfigs: jest.fn(),
      createCatalogSourceConfig: jest.fn().mockResolvedValue({}),
      getCatalogSourceConfig: jest.fn(),
      updateCatalogSourceConfig: jest.fn().mockResolvedValue({}),
      deleteCatalogSourceConfig: jest.fn(),
      deleteCatalogSourceCredentials: jest.fn(),
      previewCatalogSource: jest.fn(),
    } as unknown as ModelCatalogSettingsContextType['apiState']['api'],
  },
  refreshAPIState: jest.fn(),
  catalogSourceConfigs: null,
  catalogSourceConfigsLoaded: false,
  catalogSourceConfigsLoadError: undefined,
  refreshCatalogSourceConfigs: jest.fn(),
  catalogSources: { items: [], size: 0, pageSize: 10, nextPageToken: '' },
  catalogSourcesLoaded: true,
  catalogSourcesLoadError: undefined,
  refreshCatalogSources: jest.fn(),
  pendingSourceIds: new Map(),
  markSourcePending: jest.fn(),
  ...overrides,
});

describe('ManageSourceForm — markSourcePending on create', () => {
  beforeEach(() => {
    jest.clearAllMocks();
  });

  it('should call markSourcePending after creating a new source with a token', async () => {
    const user = userEvent.setup();
    const mockContext = createMockContext();

    render(
      <MemoryRouter>
        <ModelCatalogSettingsContext.Provider value={mockContext}>
          <ManageSourceForm isEditMode={false} />
        </ModelCatalogSettingsContext.Provider>
      </MemoryRouter>,
    );

    const nameInput = screen.getByTestId('source-name-input');
    await user.type(nameInput, 'My New Source');

    const orgInput = screen.getByTestId('organization-input');
    await user.type(orgInput, 'test-org');

    const tokenInput = screen.getByTestId('access-token-input');
    await user.type(tokenInput, 'hf_test_token');

    const submitButton = screen.getByTestId('submit-button');
    await user.click(submitButton);

    await waitFor(() => {
      expect(mockContext.apiState.api.createCatalogSourceConfig).toHaveBeenCalled();
      expect(mockContext.markSourcePending).toHaveBeenCalledWith('my_new_source', '');
    });
  });

  it('should not call markSourcePending when creating a source without a token', async () => {
    const user = userEvent.setup();
    const mockContext = createMockContext();

    render(
      <MemoryRouter>
        <ModelCatalogSettingsContext.Provider value={mockContext}>
          <ManageSourceForm isEditMode={false} />
        </ModelCatalogSettingsContext.Provider>
      </MemoryRouter>,
    );

    const nameInput = screen.getByTestId('source-name-input');
    await user.type(nameInput, 'My New Source');

    const orgInput = screen.getByTestId('organization-input');
    await user.type(orgInput, 'test-org');

    const submitButton = screen.getByTestId('submit-button');
    await user.click(submitButton);

    await waitFor(() => {
      expect(mockContext.apiState.api.createCatalogSourceConfig).toHaveBeenCalled();
      expect(mockContext.markSourcePending).not.toHaveBeenCalled();
    });
  });

  it('should call markSourcePending on edit when validation fields change', async () => {
    const user = userEvent.setup();
    const existingConfig = {
      id: 'existing_source',
      name: 'Existing Source',
      type: CatalogSourceType.HUGGING_FACE as const,
      enabled: true,
      allowedOrganization: 'old-org',
    };
    const mockContext = createMockContext({
      catalogSources: {
        items: [{ id: 'existing_source', name: 'Existing', labels: [], status: CatalogSourceStatus.AVAILABLE }],
        size: 1,
        pageSize: 10,
        nextPageToken: '',
      },
    });

    render(
      <MemoryRouter>
        <ModelCatalogSettingsContext.Provider value={mockContext}>
          <ManageSourceForm isEditMode existingSourceConfig={existingConfig} />
        </ModelCatalogSettingsContext.Provider>
      </MemoryRouter>,
    );

    const orgInput = screen.getByTestId('organization-input');
    await user.clear(orgInput);
    await user.type(orgInput, 'new-org');

    const submitButton = screen.getByTestId('submit-button');
    await user.click(submitButton);

    await waitFor(() => {
      expect(mockContext.apiState.api.updateCatalogSourceConfig).toHaveBeenCalled();
      expect(mockContext.markSourcePending).toHaveBeenCalledWith('existing_source', CatalogSourceStatus.AVAILABLE);
    });
  });
});
