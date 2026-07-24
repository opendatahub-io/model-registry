import {
  CatalogSourceConfig,
  HuggingFaceCatalogSourceConfig,
  CatalogSourceType,
} from '~/app/modelCatalogTypes';

export { CatalogSourceStatus } from '~/app/shared/types/catalogTypes';

export const CATALOG_SOURCE_TYPE_LABELS: Record<CatalogSourceType, string> = {
  [CatalogSourceType.YAML]: 'YAML file',
  [CatalogSourceType.HUGGING_FACE]: 'Hugging Face',
};

export enum ModelVisibilityBadgeColor {
  FILTERED = 'purple',
  UNFILTERED = 'grey',
}

// Type guard for Hugging Face sources
export const isHuggingFaceSource = (
  config: CatalogSourceConfig,
): config is HuggingFaceCatalogSourceConfig => config.type === CatalogSourceType.HUGGING_FACE;
