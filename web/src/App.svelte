<script lang="ts">
  let status = $state('checking…')

  async function check() {
    try {
      const res = await fetch('/healthz')
      status = res.ok ? 'ok' : `error ${res.status}`
    } catch (e) {
      status = `unreachable: ${e}`
    }
  }
  check()
</script>

<main>
  <h1>app</h1>
  <p>daemon health: <strong>{status}</strong></p>
</main>
