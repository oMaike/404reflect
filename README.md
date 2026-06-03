# 404reflect

`404reflect` e uma ferramenta Go para bug bounty que rastreia um alvo na mesma origem, identifica respostas `404 Not Found` e avisa quando a URL solicitada aparece refletida no codigo fonte da pagina.

Ela tambem pode sondar um caminho 404 aleatorio em cada diretorio descoberto, o que ajuda a detectar templates de erro que refletem qualquer URL inexistente.

## Instalacao no PowerShell

```powershell
go install github.com/oMaike/404reflect@latest
```

Se o comando `404reflect` nao for encontrado depois da instalacao, adicione o binario do Go ao `PATH`:

```powershell
$env:Path += ";$env:USERPROFILE\go\bin"
```

## Instalacao no Linux

```bash
go install github.com/oMaike/404reflect@latest
```

Se o comando `404reflect` nao for encontrado depois da instalacao, adicione o binario do Go ao `PATH`:

```bash
export PATH="$PATH:$HOME/go/bin"
```

## Uso

```powershell
404reflect -target https://example.com -depth 3 -rate 1 -user-agent "Mozilla/5.0"
```

Exemplo com wordlist e saida JSONL:

```powershell
404reflect -target https://example.com -wordlist .\paths.txt -json
```

## Flags principais

- `-target`: URL inicial do alvo.
- `-depth`: profundidade maxima do crawler.
- `-max-urls`: maximo de URLs visitadas pelo crawler.
- `-rate`: limite global de requests por segundo; `0` desativa.
- `-user-agent`: User-Agent customizado.
- `-probe-dirs`: testa um caminho 404 aleatorio em cada diretorio descoberto.
- `-wordlist`: testa nomes de uma wordlist em cada diretorio descoberto.
- `-timeout`: timeout por request.
- `-json`: imprime achados em JSONL.
- `-v`: mostra progresso no `stderr`.
