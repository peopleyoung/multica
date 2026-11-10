import { useState } from "react";
import { KeyboardAvoidingView, Platform, ScrollView, View } from "react-native";
import { SafeAreaView } from "react-native-safe-area-context";
import { router } from "expo-router";
import * as Haptics from "expo-haptics";
import { Text } from "@/components/ui/text";
import { TextField } from "@/components/ui/text-field";
import { Button } from "@/components/ui/button";
import { MulticaLogo } from "@/components/brand/multica-logo";
import { useAuthStore } from "@/data/auth-store";
import { i18n, useT } from "@/lib/i18n";
import { mapAuthError } from "@/lib/auth-error";

export default function Login() {
  const { t } = useT("auth");
  const [registering, setRegistering] = useState(false);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [email, setEmail] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const onSubmit = async () => {
    if (!username.trim() || !password) return;
    if (registering && password !== confirmation) { setError(t("password.mismatch")); return; }
    void Haptics.selectionAsync();
    setSubmitting(true);
    setError(null);
    try {
      const store = useAuthStore.getState();
      if (registering) await store.registerAccount(username, password, email || undefined);
      else await store.passwordLogin(username, password);
      setPassword(""); setConfirmation("");
      router.replace("/");
    } catch (err) {
      void Haptics.notificationAsync(Haptics.NotificationFeedbackType.Error);
      setError(mapAuthError(err, t("password.failed"), (key) => i18n.t(`auth:${key}`)));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <SafeAreaView className="flex-1 bg-background">
      <KeyboardAvoidingView
        className="flex-1"
        behavior={Platform.OS === "ios" ? "padding" : undefined}
      >
        <ScrollView contentContainerStyle={{ flexGrow: 1, justifyContent: "center", padding: 24, gap: 24 }} keyboardShouldPersistTaps="handled">
          <View className="items-center gap-3">
            <MulticaLogo size={32} />
            <View className="gap-1 items-center">
              <Text className="text-2xl font-semibold text-foreground">
                {registering ? t("password.register_title") : t("login.title")}
              </Text>

            </View>
          </View>

          <View className="gap-3">
            <TextField accessibilityLabel={t("password.username")} autoCapitalize="none" autoCorrect={false} autoComplete="username" placeholder={t("password.username")} value={username} onChangeText={setUsername} maxLength={32} editable={!submitting} />
            {registering && <Text className="text-sm text-muted-foreground">{t("password.username_help")}</Text>}
            <TextField accessibilityLabel={t("password.label")} secureTextEntry autoCapitalize="none" autoComplete={registering ? "new-password" : "current-password"} placeholder={t("password.label")} value={password} onChangeText={setPassword} onSubmitEditing={onSubmit} editable={!submitting} />
            {registering && <>
              <Text className="text-sm text-muted-foreground">{t("password.help")}</Text>
              <TextField accessibilityLabel={t("password.confirm")} secureTextEntry autoCapitalize="none" autoComplete="new-password" placeholder={t("password.confirm")} value={confirmation} onChangeText={setConfirmation} editable={!submitting} />
              <TextField accessibilityLabel={t("password.optional_email")} autoCapitalize="none" autoComplete="email" keyboardType="email-address" placeholder={t("password.optional_email")} value={email} onChangeText={setEmail} editable={!submitting} />
            </>}
            {error ? (
              <Text className="text-sm text-destructive">{error}</Text>
            ) : null}
          </View>

          <Button
            size="lg"
            disabled={submitting || !username.trim() || !password}
            onPress={onSubmit}
          >
            <Text>{submitting ? t("password.submitting") : registering ? t("password.register") : t("password.login")}</Text>
          </Button>
          <Button variant="ghost" disabled={submitting} onPress={() => { setRegistering(!registering); setPassword(""); setConfirmation(""); setError(null); }}>
            <Text>{registering ? t("password.have_account") : t("password.create_account")}</Text>
          </Button>
        </ScrollView>
      </KeyboardAvoidingView>
    </SafeAreaView>
  );
}
