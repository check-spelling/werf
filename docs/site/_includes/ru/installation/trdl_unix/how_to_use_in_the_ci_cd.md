Во-первых необходимо удостовериться, что бинарный файл `trdl` доступен для исполнения с помощью команды `type`. Данная команда печатает сообщение в поток вывода ошибок в случае, если `trdl` по каким-то причинам недоступен для использования, тем самым упрощая диагностику проблем в CI/CD системах.

Во-вторых необходимо активировать исполняемую команду `werf` в текущем экземпляре shell в CI/CD.

```shell
type trdl && . $(trdl use werf {{ include.version }} {{ include.channel }})
# Теперь можно использовать werf
werf ...
```
