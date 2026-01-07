/**
 * Load Balancing Page - Manages routing strategy, credential weights, health status, and circuit breakers
 */

import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useRoutingStrategy, STRATEGY_OPTIONS } from '@/hooks/useRoutingStrategy';
import { useSSEHealthStatus } from '@/hooks/useSSEHealthStatus';
import { useAuthStore, useNotificationStore } from '@/stores';
import { loadBalancingApi } from '@/services/api';
import { Button } from '@/components/ui/Button';
import type { CredentialWeight } from '@/services/api/loadBalancing';
import styles from './LoadBalancingPage.module.scss';

type TabType = 'weights' | 'health' | 'circuit-breaker';

export function LoadBalancingPage() {
  const { t } = useTranslation();
  const { showNotification } = useNotificationStore();
  const connectionStatus = useAuthStore((state) => state.connectionStatus);

  const [activeTab, setActiveTab] = useState<TabType>('weights');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');

  // Routing strategy
  const {
    strategy,
    loading: strategyLoading,
    updateStrategy,
    reload: reloadStrategy
  } = useRoutingStrategy();
  const [pendingStrategy, setPendingStrategy] = useState<string | null>(null);

  // SSE health status
  const {
    healthStatus,
    circuitBreakers,
    connected: sseConnected,
    error: sseError,
    reconnect: sseReconnect
  } = useSSEHealthStatus();

  // Credential weights state
  const [weights, setWeights] = useState<CredentialWeight[]>([]);
  const [editingWeights, setEditingWeights] = useState<Record<string, number>>({});
  const [savingWeights, setSavingWeights] = useState(false);

  // Action states
  const [triggeringHealthCheck, setTriggeringHealthCheck] = useState<string | null>(null);
  const [resettingStatus, setResettingStatus] = useState<string | null>(null);
  const [resettingBreaker, setResettingBreaker] = useState<string | null>(null);

  const disableControls = connectionStatus !== 'connected';

  const loadWeights = useCallback(async () => {
    try {
      const data = await loadBalancingApi.getCredentialWeights();
      setWeights(data);
      const initial: Record<string, number> = {};
      data.forEach((w) => {
        initial[w.auth_id] = w.weight;
      });
      setEditingWeights(initial);
    } catch (err: unknown) {
      console.error('Failed to load weights:', err);
    }
  }, []);

  const loadAllData = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      await Promise.all([loadWeights(), reloadStrategy()]);
    } catch (err: unknown) {
      const errorMessage = err instanceof Error ? err.message : t('notification.refresh_failed');
      setError(errorMessage);
    } finally {
      setLoading(false);
    }
  }, [loadWeights, reloadStrategy, t]);

  const handleHeaderRefresh = useCallback(async () => {
    await loadAllData();
  }, [loadAllData]);

  useHeaderRefresh(handleHeaderRefresh);

  useEffect(() => {
    loadAllData();
  }, [loadAllData]);

  // Sync pending strategy with actual strategy
  useEffect(() => {
    setPendingStrategy(null);
  }, [strategy]);

  const handleStrategyChange = async (newStrategy: string) => {
    setPendingStrategy(newStrategy);
    try {
      await updateStrategy(newStrategy as typeof strategy);
      showNotification(t('load_balancing.strategy_updated'), 'success');
    } catch (err: unknown) {
      setPendingStrategy(null);
      const errorMessage = err instanceof Error ? err.message : t('notification.update_failed');
      showNotification(`${t('notification.update_failed')}: ${errorMessage}`, 'error');
    }
  };

  const handleWeightChange = (authId: string, value: string) => {
    const numValue = parseInt(value, 10);
    if (!isNaN(numValue) && numValue >= 0) {
      setEditingWeights((prev) => ({ ...prev, [authId]: numValue }));
    }
  };

  const handleSaveWeights = async () => {
    setSavingWeights(true);
    try {
      const updates = Object.entries(editingWeights)
        .filter(([authId, weight]) => {
          const original = weights.find((w) => w.auth_id === authId);
          return original && original.weight !== weight;
        })
        .map(([auth_id, weight]) => ({ auth_id, weight }));

      if (updates.length === 0) {
        showNotification(t('load_balancing.no_changes'), 'info');
        return;
      }

      await loadBalancingApi.updateCredentialWeights(updates);
      await loadWeights();
      showNotification(t('load_balancing.weights_saved'), 'success');
    } catch (err: unknown) {
      const errorMessage = err instanceof Error ? err.message : t('notification.update_failed');
      showNotification(`${t('notification.update_failed')}: ${errorMessage}`, 'error');
    } finally {
      setSavingWeights(false);
    }
  };

  const handleTriggerHealthCheck = async (authId: string) => {
    setTriggeringHealthCheck(authId);
    try {
      await loadBalancingApi.triggerHealthCheck(authId);
      showNotification(t('load_balancing.health_check_triggered'), 'success');
    } catch (err: unknown) {
      const errorMessage = err instanceof Error ? err.message : t('notification.update_failed');
      showNotification(`${t('notification.update_failed')}: ${errorMessage}`, 'error');
    } finally {
      setTriggeringHealthCheck(null);
    }
  };

  const handleResetHealthStatus = async (authId: string) => {
    setResettingStatus(authId);
    try {
      await loadBalancingApi.resetHealthStatus(authId);
      showNotification(t('load_balancing.health_status_reset'), 'success');
    } catch (err: unknown) {
      const errorMessage = err instanceof Error ? err.message : t('notification.update_failed');
      showNotification(`${t('notification.update_failed')}: ${errorMessage}`, 'error');
    } finally {
      setResettingStatus(null);
    }
  };

  const handleResetCircuitBreaker = async (authId: string) => {
    setResettingBreaker(authId);
    try {
      await loadBalancingApi.resetCircuitBreaker(authId);
      showNotification(t('load_balancing.circuit_breaker_reset'), 'success');
    } catch (err: unknown) {
      const errorMessage = err instanceof Error ? err.message : t('notification.update_failed');
      showNotification(`${t('notification.update_failed')}: ${errorMessage}`, 'error');
    } finally {
      setResettingBreaker(null);
    }
  };

  const formatTimeAgo = (dateStr: string | null | undefined) => {
    if (!dateStr) return '-';
    const date = new Date(dateStr);
    const now = new Date();
    const diffMs = now.getTime() - date.getTime();
    const diffSec = Math.floor(diffMs / 1000);
    const diffMin = Math.floor(diffSec / 60);
    const diffHour = Math.floor(diffMin / 60);

    if (diffSec < 60) return `${diffSec}s ago`;
    if (diffMin < 60) return `${diffMin}m ago`;
    if (diffHour < 24) return `${diffHour}h ago`;
    return date.toLocaleDateString();
  };

  const getCircuitBreakerStateClass = (state: string) => {
    switch (state) {
      case 'closed':
        return styles.statusClosed;
      case 'open':
        return styles.statusOpen;
      case 'half-open':
        return styles.statusHalfOpen;
      default:
        return '';
    }
  };

  const hasWeightChanges = () => {
    return Object.entries(editingWeights).some(([authId, weight]) => {
      const original = weights.find((w) => w.auth_id === authId);
      return original && original.weight !== weight;
    });
  };

  const currentStrategy = pendingStrategy || strategy;

  const renderRoutingStrategy = () => (
    <div className={styles.strategySection}>
      <div className={styles.strategyHeader}>
        <h2 className={styles.strategyTitle}>
          <svg className={styles.sectionTitleIcon} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
            <path d="M16 3h5v5M4 20L21 3M21 16v5h-5M15 15l6 6M4 4l5 5" />
          </svg>
          {t('load_balancing.routing_strategy')}
        </h2>
        <div className={`${styles.liveIndicator} ${sseConnected ? styles.connected : styles.disconnected}`}>
          <span className={styles.liveDot} />
          <span className={styles.liveText}>
            {sseConnected ? t('load_balancing.live') : t('load_balancing.disconnected')}
          </span>
          {!sseConnected && (
            <button className={styles.reconnectBtn} onClick={sseReconnect} title={t('load_balancing.reconnect')}>
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" width="14" height="14">
                <path d="M1 4v6h6M23 20v-6h-6" />
                <path d="M20.49 9A9 9 0 0 0 5.64 5.64L1 10m22 4l-4.64 4.36A9 9 0 0 1 3.51 15" />
              </svg>
            </button>
          )}
        </div>
      </div>
      {sseError && <div className={styles.sseError}>{sseError}</div>}
      <div className={styles.strategyContent}>
        <select
          className={styles.strategySelect}
          value={currentStrategy}
          onChange={(e) => handleStrategyChange(e.target.value)}
          disabled={disableControls || strategyLoading}
        >
          {STRATEGY_OPTIONS.map((opt) => (
            <option key={opt.value} value={opt.value}>
              {t(opt.labelKey)}
            </option>
          ))}
        </select>
        <div className={styles.strategyDescription}>
          {STRATEGY_OPTIONS.find(opt => opt.value === currentStrategy) && (
            <p>{t(STRATEGY_OPTIONS.find(opt => opt.value === currentStrategy)!.descKey)}</p>
          )}
        </div>
      </div>
    </div>
  );

  const renderWeightsTab = () => (
    <div className={styles.section}>
      <div className={styles.sectionHeader}>
        <h2 className={styles.sectionTitle}>
          <svg className={styles.sectionTitleIcon} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
            <path d="M12 3v18M3 12h18M5.5 8.5l13 7M5.5 15.5l13-7" />
          </svg>
          {t('load_balancing.credential_weights')}
        </h2>
        <div className={styles.sectionActions}>
          <Button
            size="sm"
            onClick={handleSaveWeights}
            loading={savingWeights}
            disabled={disableControls || !hasWeightChanges()}
          >
            {t('common.save')}
          </Button>
          <Button
            size="sm"
            variant="secondary"
            onClick={loadWeights}
            disabled={disableControls}
          >
            {t('common.refresh')}
          </Button>
        </div>
      </div>
      <div className={styles.sectionContent}>
        {weights.length === 0 ? (
          <div className={styles.emptyState}>
            <div className={styles.emptyTitle}>{t('load_balancing.no_credentials')}</div>
            <div className={styles.emptyDesc}>{t('load_balancing.no_credentials_desc')}</div>
          </div>
        ) : (
          <div className={styles.grid}>
            {weights.map((cred) => (
              <div key={cred.auth_id} className={styles.card}>
                <div className={styles.cardHeader}>
                  <div className={styles.cardTitle}>{cred.display_name || cred.auth_id}</div>
                  <span className={styles.cardProvider}>{cred.provider}</span>
                </div>
                <div className={styles.cardBody}>
                  <div className={styles.cardRow}>
                    <span className={styles.cardLabel}>{t('load_balancing.weight')}:</span>
                    <input
                      type="number"
                      min="0"
                      className={styles.weightInput}
                      value={editingWeights[cred.auth_id] ?? cred.weight}
                      onChange={(e) => handleWeightChange(cred.auth_id, e.target.value)}
                      disabled={disableControls || cred.disabled}
                    />
                  </div>
                  <div className={styles.cardRow}>
                    <span className={styles.cardLabel}>{t('load_balancing.status')}:</span>
                    <span className={`${styles.statusBadge} ${cred.disabled ? styles.statusDisabled : styles.statusHealthy}`}>
                      {cred.disabled ? t('load_balancing.disabled') : t('load_balancing.enabled')}
                    </span>
                  </div>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );

  const renderHealthTab = () => (
    <div className={styles.section}>
      <div className={styles.sectionHeader}>
        <h2 className={styles.sectionTitle}>
          <svg className={styles.sectionTitleIcon} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
            <path d="M22 12h-4l-3 9L9 3l-3 9H2" />
          </svg>
          {t('load_balancing.health_status')}
          {sseConnected && <span className={styles.liveTag}>{t('load_balancing.live')}</span>}
        </h2>
      </div>
      <div className={styles.sectionContent}>
        {healthStatus.length === 0 ? (
          <div className={styles.emptyState}>
            <div className={styles.emptyTitle}>{t('load_balancing.no_health_data')}</div>
            <div className={styles.emptyDesc}>{t('load_balancing.no_health_data_desc')}</div>
          </div>
        ) : (
          <div className={styles.grid}>
            {healthStatus.map((status) => (
              <div key={status.auth_id} className={styles.card}>
                <div className={styles.cardHeader}>
                  <div className={styles.cardTitle}>{status.display_name || status.auth_id}</div>
                  <span className={styles.cardProvider}>{status.provider}</span>
                </div>
                <div className={styles.cardBody}>
                  <div className={styles.cardRow}>
                    <span className={styles.cardLabel}>{t('load_balancing.health')}:</span>
                    <span className={`${styles.statusBadge} ${status.healthy ? styles.statusHealthy : styles.statusUnhealthy}`}>
                      <span className={`${styles.indicatorDot} ${status.healthy ? styles.dotGreen : styles.dotRed}`} />
                      {status.healthy ? t('load_balancing.healthy') : t('load_balancing.unhealthy')}
                    </span>
                  </div>
                  <div className={styles.cardRow}>
                    <span className={styles.cardLabel}>{t('load_balancing.auth_type')}:</span>
                    <span className={styles.cardValue}>{status.auth_type}</span>
                  </div>
                  <div className={styles.cardRow}>
                    <span className={styles.cardLabel}>{t('load_balancing.circuit_state')}:</span>
                    <span className={`${styles.statusBadge} ${getCircuitBreakerStateClass(status.circuit_state)}`}>
                      {status.circuit_state.toUpperCase()}
                    </span>
                  </div>
                  <div className={styles.cardRow}>
                    <span className={styles.cardLabel}>{t('load_balancing.last_check')}:</span>
                    <span className={styles.timeAgo}>{formatTimeAgo(status.last_check)}</span>
                  </div>
                  {status.consecutive_failures > 0 && (
                    <div className={styles.cardRow}>
                      <span className={styles.cardLabel}>{t('load_balancing.failures')}:</span>
                      <span className={styles.cardValue}>{status.consecutive_failures}</span>
                    </div>
                  )}
                  {status.last_error && (
                    <div className={styles.errorMessage}>{status.last_error}</div>
                  )}
                </div>
                <div className={styles.cardActions}>
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={() => handleTriggerHealthCheck(status.auth_id)}
                    loading={triggeringHealthCheck === status.auth_id}
                    disabled={disableControls}
                  >
                    {t('load_balancing.trigger_check')}
                  </Button>
                  {!status.healthy && (
                    <Button
                      size="sm"
                      variant="secondary"
                      onClick={() => handleResetHealthStatus(status.auth_id)}
                      loading={resettingStatus === status.auth_id}
                      disabled={disableControls}
                    >
                      {t('load_balancing.reset_status')}
                    </Button>
                  )}
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );

  const renderCircuitBreakerTab = () => (
    <div className={styles.section}>
      <div className={styles.sectionHeader}>
        <h2 className={styles.sectionTitle}>
          <svg className={styles.sectionTitleIcon} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
            <circle cx="12" cy="12" r="10" />
            <path d="M12 6v6l4 2" />
          </svg>
          {t('load_balancing.circuit_breakers')}
          {sseConnected && <span className={styles.liveTag}>{t('load_balancing.live')}</span>}
        </h2>
      </div>
      <div className={styles.sectionContent}>
        {circuitBreakers.length === 0 ? (
          <div className={styles.emptyState}>
            <div className={styles.emptyTitle}>{t('load_balancing.no_circuit_breakers')}</div>
            <div className={styles.emptyDesc}>{t('load_balancing.no_circuit_breakers_desc')}</div>
          </div>
        ) : (
          <div className={styles.grid}>
            {circuitBreakers.map((cb) => (
              <div key={cb.auth_id} className={styles.card}>
                <div className={styles.cardHeader}>
                  <div className={styles.cardTitle}>{cb.display_name || cb.auth_id}</div>
                  <span className={styles.cardProvider}>{cb.provider}</span>
                </div>
                <div className={styles.cardBody}>
                  <div className={styles.cardRow}>
                    <span className={styles.cardLabel}>{t('load_balancing.state')}:</span>
                    <span className={`${styles.statusBadge} ${getCircuitBreakerStateClass(cb.state)}`}>
                      <span className={`${styles.indicatorDot} ${cb.state === 'closed' ? styles.dotGreen : cb.state === 'open' ? styles.dotRed : styles.dotYellow}`} />
                      {cb.state.toUpperCase()}
                    </span>
                  </div>
                  <div className={styles.cardRow}>
                    <span className={styles.cardLabel}>{t('load_balancing.failure_count')}:</span>
                    <span className={styles.cardValue}>{cb.failure_count}</span>
                  </div>
                  <div className={styles.cardRow}>
                    <span className={styles.cardLabel}>{t('load_balancing.success_count')}:</span>
                    <span className={styles.cardValue}>{cb.success_count}</span>
                  </div>
                  {cb.last_failure && (
                    <div className={styles.cardRow}>
                      <span className={styles.cardLabel}>{t('load_balancing.last_failure')}:</span>
                      <span className={styles.timeAgo}>{formatTimeAgo(cb.last_failure)}</span>
                    </div>
                  )}
                  {cb.state === 'open' && cb.next_retry && (
                    <div className={styles.cardRow}>
                      <span className={styles.cardLabel}>{t('load_balancing.next_retry')}:</span>
                      <span className={styles.timeAgo}>{formatTimeAgo(cb.next_retry)}</span>
                    </div>
                  )}
                </div>
                {cb.state !== 'closed' && (
                  <div className={styles.cardActions}>
                    <Button
                      size="sm"
                      variant="secondary"
                      onClick={() => handleResetCircuitBreaker(cb.auth_id)}
                      loading={resettingBreaker === cb.auth_id}
                      disabled={disableControls}
                    >
                      {t('load_balancing.reset')}
                    </Button>
                  </div>
                )}
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );

  return (
    <div className={styles.container}>
      <div className={styles.pageHeader}>
        <h1 className={styles.pageTitle}>{t('load_balancing.title')}</h1>
        <p className={styles.description}>{t('load_balancing.description')}</p>
      </div>

      {error && <div className={styles.errorBox}>{error}</div>}

      {renderRoutingStrategy()}

      <div className={styles.section}>
        <div className={styles.tabs}>
          <button
            className={`${styles.tab} ${activeTab === 'weights' ? styles.active : ''}`}
            onClick={() => setActiveTab('weights')}
          >
            {t('load_balancing.tab_weights')}
          </button>
          <button
            className={`${styles.tab} ${activeTab === 'health' ? styles.active : ''}`}
            onClick={() => setActiveTab('health')}
          >
            {t('load_balancing.tab_health')}
          </button>
          <button
            className={`${styles.tab} ${activeTab === 'circuit-breaker' ? styles.active : ''}`}
            onClick={() => setActiveTab('circuit-breaker')}
          >
            {t('load_balancing.tab_circuit_breaker')}
          </button>
        </div>
      </div>

      {loading ? (
        <div className={styles.loading}>{t('common.loading')}</div>
      ) : (
        <>
          {activeTab === 'weights' && renderWeightsTab()}
          {activeTab === 'health' && renderHealthTab()}
          {activeTab === 'circuit-breaker' && renderCircuitBreakerTab()}
        </>
      )}
    </div>
  );
}
