// Constants for AI model and provider configuration

export interface AIProviderConfig {
  defaultModel: string
  apiUrl: string
  apiName: string
}

// Get friendly AI model display name
export function getModelDisplayName(modelId: string): string {
  switch (modelId.toLowerCase()) {
    case 'deepseek':
      return 'DeepSeek'
    case 'qwen':
      return 'Qwen'
    case 'claude':
      return 'Claude'
    default:
      return modelId.toUpperCase()
  }
}

// Extract name part after underscore
export function getShortName(fullName: string): string {
  const parts = fullName.split('_')
  return parts.length > 1 ? parts[parts.length - 1] : fullName
}

// AI Provider configuration - default models and API links
export const AI_PROVIDER_CONFIG: Record<string, AIProviderConfig> = {}


// Helper function to get exchange display name from exchange ID (UUID)
export function getExchangeDisplayName(exchangeId: string | undefined, exchanges: { id: string; exchange_type?: string; name: string; account_name?: string }[]): string {
  if (!exchangeId) return 'Unknown'
  const exchange = exchanges.find(e => e.id === exchangeId)
  if (!exchange) return exchangeId.substring(0, 8).toUpperCase() + '...' // Show truncated UUID if not found
  const typeName = exchange.exchange_type?.toUpperCase() || exchange.name
  return exchange.account_name ? `${typeName} - ${exchange.account_name}` : typeName
}

// Helper function to check if exchange is a perp-dex type (wallet-based)
export function isPerpDexExchange(exchangeType: string | undefined): boolean {
  if (!exchangeType) return false
  const perpDexTypes = ['hyperliquid', 'lighter', 'aster']
  return perpDexTypes.includes(exchangeType.toLowerCase())
}

// Helper function to get wallet address for perp-dex exchanges
export function getWalletAddress(exchange: { exchange_type?: string; hyperliquidWalletAddr?: string; lighterWalletAddr?: string; asterSigner?: string } | undefined): string | undefined {
  if (!exchange) return undefined
  const type = exchange.exchange_type?.toLowerCase()
  switch (type) {
    case 'hyperliquid':
      return exchange.hyperliquidWalletAddr
    case 'lighter':
      return exchange.lighterWalletAddr
    case 'aster':
      return exchange.asterSigner
    default:
      return undefined
  }
}

// Helper function to truncate wallet address for display
export function truncateAddress(address: string, startLen = 6, endLen = 4): string {
  if (address.length <= startLen + endLen + 3) return address
  return `${address.slice(0, startLen)}...${address.slice(-endLen)}`
}
