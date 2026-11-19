import { useCallback, useEffect, useState } from 'react';
import { SpeakerService, roleIsUnidentified, type Role } from '@/services';

export const roleColorTokens = ['role-1', 'role-2', 'role-3', 'role-4', 'role-5', 'role-6', 'role-7', 'role-8', 'accent', 'neutral'];

export const useRolePreferences = () => {
  const [prefix, setPrefix] = useState<string | null>(null);
  const [roles, setRoles] = useState<Role[]>([]);
  const [roleId, setRoleId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);

  const loadRoles = useCallback(async () => (await SpeakerService.listRoles())
    .filter(({ id, isSystem }) => !isSystem && !roleIsUnidentified(id)), []);

  useEffect(() => {
    let active = true;
    let revision = 0;
    const refreshRoles = async () => {
      const request = ++revision;
      try {
        const result = await loadRoles();
        if (active && request === revision) setRoles(result);
      } catch {
        if (active && request === revision) setFailed(true);
      }
    };
    const off = SpeakerService.onRolesChanged(() => void refreshRoles());
    void refreshRoles();
    void SpeakerService.getRoleNamePrefix().then((value) => {
      if (active) setPrefix(value);
    }).catch(() => { if (active) setFailed(true); });
    return () => { active = false; off(); };
  }, [loadRoles]);

  const savePrefix = async () => {
    if (prefix === null || busy || [...prefix.trim()].length > 20) return;
    setBusy(true);
    setFailed(false);
    try {
      await SpeakerService.setRoleNamePrefix(prefix);
      setPrefix(await SpeakerService.getRoleNamePrefix());
    } catch { setFailed(true); }
    finally { setBusy(false); }
  };

  const updateColor = async (token: string | null) => {
    if (roleId === null || token === null || !roleColorTokens.includes(token) || busy) return;
    setBusy(true);
    setFailed(false);
    try {
      await SpeakerService.setRoleColor(roleId, token);
      setRoles((current) => current.map((role) => role.id === roleId ? { ...role, colorToken: token } : role));
    } catch { setFailed(true); }
    finally { setBusy(false); }
  };

  const role = roles.find(({ id }) => id === roleId);
  return { prefix, setPrefix, savePrefix, roles, role, setRoleId, updateColor, busy, failed };
};
