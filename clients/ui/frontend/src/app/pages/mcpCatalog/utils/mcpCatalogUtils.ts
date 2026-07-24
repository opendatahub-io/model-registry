import type { McpCatalogFiltersState } from '~/app/pages/mcpCatalog/types/mcpCatalogFilterOptions';
import { BACKEND_TO_FRONTEND_FILTER_KEY, MCP_FILTER_KEYS } from '~/app/pages/mcpCatalog/const';
import {
  MetadataType,
  type McpCustomProperties,
  type McpDeploymentMode,
  type McpEndpoints,
  type McpSecurityIndicator,
} from '~/app/mcpServerCatalogTypes';
import { hasFiltersApplied, stringFiltersToFilterQuery } from '~/app/shared/components/catalog';

export const isMcpRemoteDeploymentMode = (mode?: McpDeploymentMode): boolean => mode === 'remote';

export const getMcpServerPrimaryEndpoint = (
  endpoints?: McpEndpoints | null,
): string | undefined => {
  if (!endpoints) {
    return undefined;
  }
  const http = endpoints.http?.trim();
  if (http) {
    return http;
  }
  const sse = endpoints.sse?.trim();
  if (sse) {
    return sse;
  }
  return undefined;
};

const SECURITY_INDICATOR_LABELS: Record<keyof McpSecurityIndicator, string> = {
  verifiedSource: 'Verified source',
  secureEndpoint: 'Secure endpoint',
  sast: 'SAST',
  readOnlyTools: 'Read only tools',
};

const SECURITY_INDICATOR_KEYS: (keyof McpSecurityIndicator)[] = [
  'verifiedSource',
  'secureEndpoint',
  'sast',
  'readOnlyTools',
];

export const getSecurityIndicatorLabels = (
  securityIndicators?: McpSecurityIndicator | null,
): string[] => {
  if (!securityIndicators) {
    return [];
  }
  return SECURITY_INDICATOR_KEYS.filter((key) => Boolean(securityIndicators[key])).map(
    (key) => SECURITY_INDICATOR_LABELS[key],
  );
};

export const hasMcpFiltersApplied = (
  filters: McpCatalogFiltersState,
  searchQuery: string,
): boolean => hasFiltersApplied(filters, MCP_FILTER_KEYS, searchQuery);

const FRONTEND_TO_BACKEND_FILTER_KEY: Record<string, string> = Object.fromEntries(
  Object.entries(BACKEND_TO_FRONTEND_FILTER_KEY).map(([backend, frontend]) => [frontend, backend]),
);

export function mcpFiltersToFilterQuery(filters: McpCatalogFiltersState): string {
  return stringFiltersToFilterQuery(filters, FRONTEND_TO_BACKEND_FILTER_KEY);
}

export enum SupportTier {
  COMMUNITY = 'communitySupported',
  PARTNER = 'partnerSupported',
  RED_HAT = 'redHatSupported',
}

const SUPPORT_TIER_DISPLAY: Record<SupportTier, string> = {
  [SupportTier.COMMUNITY]: 'Community supported',
  [SupportTier.PARTNER]: 'Partner supported',
  [SupportTier.RED_HAT]: 'Red Hat supported',
};

const SUPPORT_TIER_VALUES = new Set<string>(Object.values(SupportTier));

const isSupportTier = (value: string): value is SupportTier => SUPPORT_TIER_VALUES.has(value);

export const getSupportTierFromCustomProperties = (
  customProperties?: McpCustomProperties,
): SupportTier | undefined => {
  if (!customProperties?.supportTier) {
    return undefined;
  }
  const prop = customProperties.supportTier;
  if (prop.metadataType !== MetadataType.STRING) {
    return undefined;
  }
  if (isSupportTier(prop.string_value)) {
    return prop.string_value;
  }
  return undefined;
};

export const getSupportTierDisplayName = (tier: SupportTier): string => SUPPORT_TIER_DISPLAY[tier];
